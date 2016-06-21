package mongodb

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-check"
	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/mgo.v2"
	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/mgo.v2/bson"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/attempt"
	"github.com/flynn/flynn/pkg/sirenia/state"
)

// Hook gocheck up to the "go test" runner
func Test(t *testing.T) { TestingT(t) }

type MongoDBSuite struct{}

var _ = Suite(&MongoDBSuite{})

func (MongoDBSuite) TestSingletonPrimary(c *C) {
	p := NewProcess()
	p.ID = "node1"
	p.Singleton = true
	p.Password = "password"
	p.DataDir = c.MkDir()
	p.Port = "7500"
	p.OpTimeout = 30 * time.Second
	topology := &state.State{Primary: instance(p)}
	err := p.Reconfigure(&state.Config{Role: state.RolePrimary, State: topology})
	c.Assert(err, IsNil)

	err = p.Start()
	c.Assert(err, IsNil)
	defer p.Stop()

	session := connect(c, p, "")
	/*_, err = session.Exec("CREATE DATABASE test")*/
	session.Close()
	c.Assert(err, IsNil)

	err = p.Stop()
	c.Assert(err, IsNil)

	// ensure that we can start a new instance from the same directory
	p = NewProcess()
	p.ID = "node1"
	p.Singleton = true
	p.Password = "password"
	p.DataDir = c.MkDir()
	p.Port = "7500"
	p.OpTimeout = 30 * time.Second
	err = p.Reconfigure(&state.Config{Role: state.RolePrimary, State: topology})
	c.Assert(err, IsNil)
	c.Assert(p.Start(), IsNil)
	defer p.Stop()

	session = connect(c, p, "")
	/*_, err = session.Exec("CREATE DATABASE foo")*/
	session.Close()
	c.Assert(err, IsNil)

	err = p.Stop()
	c.Assert(err, IsNil)
}

func instance(p *Process) *discoverd.Instance {
	return &discoverd.Instance{
		ID:   p.ID,
		Addr: fmt.Sprintf("127.0.0.1:%d", MustAtoi(p.Port)),
		Meta: map[string]string{
			"MONGODB_ID": p.ID,
		},
	}
}

func connect(c *C, p *Process, database string) *mgo.Session {
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{fmt.Sprintf("127.0.0.1:%d", MustAtoi(p.Port))},
		Database: database,
		Direct:   true,
	})
	c.Assert(err, IsNil)
	return session
}

func Config(role state.Role, upstream, downstream *Process, topology *state.State) *state.Config {
	c := &state.Config{Role: role, State: topology}
	if upstream != nil {
		c.Upstream = instance(upstream)
	}
	if downstream != nil {
		c.Downstream = instance(downstream)
	}
	return c
}

var queryAttempts = attempt.Strategy{
	Min:   5,
	Total: 30 * time.Second,
	Delay: 200 * time.Millisecond,
}

func assertDownstream(c *C, session *mgo.Session, downstream *Process) {
	var entry struct {
		Retval struct {
			Members []struct {
				Name string `bson:"name"`
			} `bson:"members"`
		} `bson:"retval"`
	}
	if err := session.Run(bson.D{{"eval", `rs.status()`}}, &entry); err != nil {
		c.Assert(err, IsNil)
	}

	var names []string
	for _, member := range entry.Retval.Members {
		if member.Name == downstream.Host+":"+downstream.Port {
			return
		}
	}
	c.Fatalf("downstream not in member list: %+v", names)
}

func waitRow(c *C, session *mgo.Session, n int) {
	err := queryAttempts.Run(func() error {
		var doc Doc
		if err := session.DB("db0").C("test").Find(bson.M{"n": n}).One(&doc); err != nil {
			return err
		} else if doc.N != n {
			return fmt.Errorf("row n mismatch: %d != %d", n, doc.N)
		}
		return nil
	})
	c.Assert(err, IsNil)
}

func insertDoc(c *C, session *mgo.Session, n int) {
	c.Assert(session.DB("db0").C("test").Insert(&Doc{N: n}), IsNil)
}

func waitReadWrite(c *C, session *mgo.Session) {
	err := queryAttempts.Run(func() error {
		var entry struct {
			Retval struct {
				IsMaster bool `bson:"ismaster"`
			} `bson:"retval"`
		}
		if err := session.Run(bson.D{{"eval", `db.isMaster()`}}, &entry); err != nil {
			return err
		}
		if !entry.Retval.IsMaster {
			return errors.New("not master")
		}
		return nil
	})
	c.Assert(err, IsNil)
}

var syncAttempts = attempt.Strategy{
	Min:   5,
	Total: 30 * time.Second,
	Delay: 200 * time.Millisecond,
}

func waitReplSync(c *C, p *Process, n int) {
	id := fmt.Sprintf("node%d", n)
	err := syncAttempts.Run(func() error {
		info, err := p.Info()
		if err != nil {
			return err
		}
		if info.SyncedDownstream == nil || info.SyncedDownstream.ID != id {
			return errors.New("downstream not synced")
		}
		return nil
	})
	c.Assert(err, IsNil, Commentf("up:%s down:%s", p.ID, id))
}

func (MongoDBSuite) TestIntegration_TwoNodeSync(c *C) {
	node1 := NewTestProcess(c, 1)
	node2 := NewTestProcess(c, 2)

	topology := &state.State{Primary: instance(node1), Sync: instance(node2)}
	println("DBG?", node1 == nil)

	// Start a primary.
	err := node1.Reconfigure(Config(state.RolePrimary, nil, node2, topology))
	c.Assert(err, IsNil)
	c.Assert(node1.Start(), IsNil)
	defer node1.Stop()

	srv1 := NewHTTPServer(c, node1)
	defer srv1.Close()

	// Start a sync
	err = node2.Reconfigure(Config(state.RoleSync, node1, nil, topology))
	c.Assert(err, IsNil)
	c.Assert(node2.Start(), IsNil)
	defer node2.Stop()

	srv2 := NewHTTPServer(c, node2)
	defer srv2.Close()

	// check it catches up
	waitReplSync(c, node1, 2)

	// Write to the master.
	session1 := connect(c, node1, "db0")
	defer session1.Close()
	insertDoc(c, session1, 1)

	// Read from the slave.
	session2 := connect(c, node2, "db0")
	session2.SetMode(mgo.Secondary, true)
	defer session2.Close()
	waitRow(c, session2, 1)
}

func (MongoDBSuite) TestIntegration_FourNode(c *C) {
	node1 := NewTestProcess(c, 1)
	node2 := NewTestProcess(c, 2)
	node3 := NewTestProcess(c, 3)
	node4 := NewTestProcess(c, 4)

	topology := &state.State{
		Primary: instance(node1),
		Sync:    instance(node2),
		Async:   []*discoverd.Instance{instance(node3), instance(node4)},
	}

	// Start a primary
	err := node1.Reconfigure(Config(state.RolePrimary, nil, node2, topology))
	c.Assert(err, IsNil)
	c.Assert(node1.Start(), IsNil)
	defer node1.Stop()

	srv1 := NewHTTPServer(c, node1)
	defer srv1.Close()

	// try to write to primary and make sure it's read-only
	db1 := connect(c, node1, "db0")
	defer db1.Close()

	// Start a sync
	err = node2.Reconfigure(Config(state.RoleSync, node1, node3, topology))
	c.Assert(err, IsNil)
	c.Assert(node2.Start(), IsNil)
	defer node2.Stop()

	srv2 := NewHTTPServer(c, node2)
	defer srv2.Close()

	// check it catches up
	waitReplSync(c, node1, 2)

	// try to query primary until it comes up as read-write
	waitReadWrite(c, db1)

	for _, n := range []*Process{node1, node2} {
		pos, err := n.XLogPosition()
		c.Assert(err, IsNil)
		c.Assert(pos, Not(Equals), "")
		c.Assert(pos, Not(Equals), "master-bin.000000/0")
	}

	// make sure the sync is listed as sync and remote_write is enabled
	assertDownstream(c, db1, node2)

	// create a table and a row
	insertDoc(c, db1, 1)

	// query the sync and see the database
	db2 := connect(c, node2, "db0")
	db2.SetMode(mgo.Secondary, true)
	defer db2.Close()
	waitRow(c, db2, 1)

	// Start an async
	err = node3.Reconfigure(Config(state.RoleAsync, node2, node4, topology))
	c.Assert(err, IsNil)
	c.Assert(node3.Start(), IsNil)
	defer node3.Stop()

	srv3 := NewHTTPServer(c, node3)
	defer srv3.Close()

	// check it catches up
	waitReplSync(c, node2, 3)

	db3 := connect(c, node3, "db0")
	db3.SetMode(mgo.Secondary, true)
	defer db3.Close()

	// check that data replicated successfully
	waitRow(c, db3, 1)
	assertDownstream(c, db1, node3)

	// Start a second async
	err = node4.Reconfigure(Config(state.RoleAsync, node3, nil, topology))
	c.Assert(err, IsNil)
	c.Assert(node4.Start(), IsNil)
	defer node4.Stop()

	srv4 := NewHTTPServer(c, node4)
	defer srv4.Close()

	// check it catches up
	waitReplSync(c, node3, 4)

	db4 := connect(c, node4, "db0")
	db4.SetMode(mgo.Secondary, true)
	defer db4.Close()

	// check that data replicated successfully
	waitRow(c, db4, 1)
	assertDownstream(c, db1, node4)

	// promote node2 to primary
	topology = &state.State{
		Primary: instance(node2),
		Sync:    instance(node3),
		Async:   []*discoverd.Instance{instance(node4)},
	}
	c.Assert(node1.Stop(), IsNil)
	err = node2.Reconfigure(Config(state.RolePrimary, nil, node3, topology))
	c.Assert(err, IsNil)
	err = node3.Reconfigure(Config(state.RoleSync, node2, node4, topology))
	c.Assert(err, IsNil)

	// Reconnect to node 2 as primary.
	db2.Close()
	db2 = connect(c, node2, "db0")
	defer db2.Close()

	// wait for recovery and read-write transactions to come up
	waitReplSync(c, node2, 3)
	waitReadWrite(c, db2)

	// check replication of each node
	assertDownstream(c, db2, node3)
	assertDownstream(c, db2, node4)

	// write to primary and ensure data propagates to followers
	insertDoc(c, db2, 2)
	db2.Close()

	db3.SetMode(mgo.Secondary, true)
	waitRow(c, db3, 2)
	db4.SetMode(mgo.Secondary, true)
	waitRow(c, db4, 2)

	topology = &state.State{
		Primary: instance(node3),
		Sync:    instance(node4),
	}

	//  promote node3 to primary
	c.Assert(node2.Stop(), IsNil)
	err = node3.Reconfigure(Config(state.RolePrimary, nil, node4, topology))
	c.Assert(err, IsNil)
	err = node4.Reconfigure(Config(state.RoleSync, node3, nil, topology))

	// Reconnect to node 3 as primary.
	db3.Close()
	db3 = connect(c, node3, "db0")
	defer db3.Close()

	// check replication
	waitReplSync(c, node3, 4)
	waitReadWrite(c, db3)
	assertDownstream(c, db3, node4)
	insertDoc(c, db3, 3)
}

func (MongoDBSuite) TestRemoveNodes(c *C) {
	node1 := NewTestProcess(c, 1)
	node2 := NewTestProcess(c, 2)
	node3 := NewTestProcess(c, 3)
	node4 := NewTestProcess(c, 4)

	topology := &state.State{
		Primary: instance(node1),
		Sync:    instance(node2),
		Async:   []*discoverd.Instance{instance(node3), instance(node4)},
	}

	// start a chain of four nodes
	err := node1.Reconfigure(Config(state.RolePrimary, nil, node2, topology))
	c.Assert(err, IsNil)
	c.Assert(node1.Start(), IsNil)
	defer node1.Stop()

	srv1 := NewHTTPServer(c, node1)
	defer srv1.Close()

	err = node2.Reconfigure(Config(state.RoleSync, node1, nil, topology))
	c.Assert(err, IsNil)
	c.Assert(node2.Start(), IsNil)
	defer node2.Stop()

	srv2 := NewHTTPServer(c, node2)
	defer srv2.Close()

	err = node3.Reconfigure(Config(state.RoleAsync, node2, nil, topology))
	c.Assert(err, IsNil)
	c.Assert(node3.Start(), IsNil)
	defer node3.Stop()

	srv3 := NewHTTPServer(c, node3)
	defer srv3.Close()

	err = node4.Reconfigure(Config(state.RoleAsync, node3, nil, topology))
	c.Assert(err, IsNil)
	c.Assert(node4.Start(), IsNil)
	defer node4.Stop()

	srv4 := NewHTTPServer(c, node4)
	defer srv4.Close()

	// wait for cluster to come up
	node1Conn := connect(c, node1, "db0")
	defer node1Conn.Close()
	db4 := connect(c, node4, "db0")
	defer db4.Close()
	waitReadWrite(c, node1Conn)
	insertDoc(c, node1Conn, 1)
	waitRow(c, db4, 1)
	db4.Close()

	// remove first async
	c.Assert(node3.Stop(), IsNil)
	// reconfigure second async
	err = node4.Reconfigure(Config(state.RoleAsync, node2, nil, topology))
	c.Assert(err, IsNil)

	// run query
	db4 = connect(c, node4, "db0")
	defer db4.Close()
	insertDoc(c, node1Conn, 2)
	waitRow(c, db4, 2)
	db4.Close()

	// remove sync and promote node4 to sync
	c.Assert(node2.Stop(), IsNil)
	c.Assert(node1.Reconfigure(Config(state.RolePrimary, nil, node4, topology)), IsNil)
	c.Assert(node4.Reconfigure(Config(state.RoleSync, node1, nil, topology)), IsNil)

	waitReadWrite(c, node1Conn)
	insertDoc(c, node1Conn, 3)
	db4 = connect(c, node4, "db0")
	defer db4.Close()
	waitRow(c, db4, 3)
}

// newPort represents the starting port when allocating new ports.
var newPort uint32 = 7500

func NewTestProcess(c *C, n uint32) *Process {
	p := NewProcess()
	p.ID = fmt.Sprintf("node%d", n)
	p.DataDir = c.MkDir()
	p.Port = strconv.Itoa(int(atomic.AddUint32(&newPort, 2)))
	p.Password = "password"
	p.OpTimeout = 30 * time.Second
	p.Logger = p.Logger.New("id", p.ID, "port", p.Port)
	return p
}

// HTTPServer is a wrapper for http.Server that provides the ability to close the listener.
type HTTPServer struct {
	*http.Server
	ln net.Listener
}

// NewHTTPServer returns a new, running HTTP server attached to a process.
func NewHTTPServer(c *C, p *Process) *HTTPServer {
	h := NewHandler()
	h.Process = p

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", MustAtoi(p.Port)+1))
	c.Assert(err, IsNil)

	s := &HTTPServer{
		Server: &http.Server{
			Handler: h,
		},
		ln: ln,
	}
	go s.Serve(ln)

	return s
}

// Close closes the server's listener.
func (s *HTTPServer) Close() error { s.ln.Close(); return nil }

// MustAtoi converts a string into an integer. Panic on error.
func MustAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
}

type Doc struct {
	N int `bson:"n"`
}
