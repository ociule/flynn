package mongodb

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/mgo.v2"
	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/mgo.v2/bson"
	mongodbxlog "github.com/flynn/flynn/appliance/mongodb/xlog"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/shutdown"
	"github.com/flynn/flynn/pkg/sirenia/client"
	"github.com/flynn/flynn/pkg/sirenia/state"
	"github.com/flynn/flynn/pkg/sirenia/xlog"
)

const (
	DefaultHost        = "localhost"
	DefaultPort        = "27017"
	DefaultBinDir      = "/usr/bin"
	DefaultDataDir     = "/data"
	DefaultPassword    = ""
	DefaultOpTimeout   = 5 * time.Minute
	DefaultReplTimeout = 1 * time.Minute

	BinName    = "mongod"
	ConfigName = "mongod.conf"

	checkInterval = 1000 * time.Millisecond
)

var (
	// ErrRunning is returned when starting an already running process.
	ErrRunning = errors.New("process already running")

	// ErrStopped is returned when stopping an already stopped process.
	ErrStopped = errors.New("process already stopped")

	ErrNoReplicationStatus = errors.New("no replication status")
)

// Process represents a MongoDB process.
type Process struct {
	mtx sync.Mutex

	events chan state.DatabaseEvent

	// Replication configuration
	configValue   atomic.Value // *Config
	configApplied bool

	runningValue          atomic.Value // bool
	syncedDownstreamValue atomic.Value // *discoverd.Instance

	ID           string
	Singleton    bool
	Host         string
	Port         string
	BinDir       string
	DataDir      string
	Password     string
	ServerID     uint32
	OpTimeout    time.Duration
	ReplTimeout  time.Duration
	WaitUpstream bool

	Logger log15.Logger

	// cmd is the running system command.
	cmd *Cmd

	// cancelSyncWait cancels the goroutine that is waiting for
	// the downstream to catch up, if running.
	cancelSyncWait func()
}

// NewProcess returns a new instance of Process.
func NewProcess() *Process {
	p := &Process{
		Host:        DefaultHost,
		Port:        DefaultPort,
		BinDir:      DefaultBinDir,
		DataDir:     DefaultDataDir,
		Password:    DefaultPassword,
		OpTimeout:   DefaultOpTimeout,
		ReplTimeout: DefaultReplTimeout,
		Logger:      log15.New("app", "mongodb"),

		events:         make(chan state.DatabaseEvent, 1),
		cancelSyncWait: func() {},
	}
	p.runningValue.Store(false)
	p.configValue.Store((*state.Config)(nil))
	p.events <- state.DatabaseEvent{}
	return p
}

func (p *Process) running() bool         { return p.runningValue.Load().(bool) }
func (p *Process) config() *state.Config { return p.configValue.Load().(*state.Config) }

func (p *Process) syncedDownstream() *discoverd.Instance {
	if downstream, ok := p.syncedDownstreamValue.Load().(*discoverd.Instance); ok {
		return downstream
	}
	return nil
}

func (p *Process) ConfigPath() string { return filepath.Join(p.DataDir, "mongod.conf") }

func (p *Process) Reconfigure(config *state.Config) error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	switch config.Role {
	case state.RolePrimary:
		if !p.Singleton && config.Downstream == nil {
			return errors.New("missing downstream peer")
		}
	case state.RoleSync, state.RoleAsync:
		if config.Upstream == nil {
			return fmt.Errorf("missing upstream peer")
		}
	case state.RoleNone:
	default:
		return fmt.Errorf("unknown role %v", config.Role)
	}

	if !p.running() {
		p.configValue.Store(config)
		p.configApplied = false
		return nil
	}

	return p.reconfigure(config)
}

func (p *Process) Start() error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.running() {
		return errors.New("process already running")
	}
	if p.config() == nil {
		return errors.New("unconfigured process")
	}
	if p.config().Role == state.RoleNone {
		return errors.New("start attempted with role 'none'")
	}

	return p.reconfigure(nil)
}

func (p *Process) Stop() error {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if !p.running() {
		return errors.New("process already stopped")
	}
	return p.stop()
}

func (p *Process) Ready() <-chan state.DatabaseEvent {
	return p.events
}

func (p *Process) XLog() xlog.XLog {
	return mongodbxlog.XLog{}
}

func (p *Process) getReplConfig() (*replSetConfig, error) {
	// Connect to local server.
	session, err := p.connectLocal()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	session.SetMode(mgo.Monotonic, true)

	// Retrieve replica set configuration.
	var result struct {
		Config replSetConfig `bson:"config"`
	}
	if session.Run(bson.D{{"replSetGetConfig", 1}}, &result); err != nil {
		return nil, err
	}
	return &result.Config, nil
}

func (p *Process) setReplConfig(config replSetConfig) error {
	// Connect to local server.
	session, err := p.connectLocal()
	if err != nil {
		return err
	}
	defer session.Close()
	session.SetMode(mgo.Monotonic, true)
	if session.Run(bson.D{{"replSetReconfig", config}, {"force", true}}, nil); err != nil {
		return err
	}
	return nil
}

func (p *Process) replSetConfigFromState(clusterState *state.State) replSetConfig {
	//TODO(jpg): Investigate stable IDs, not sure if non-stable IDs will screw with MongoDB
	members := []replSetMember{{ID: 0, Host: clusterState.Primary.Addr, Priority: 1}}
	// If we aren't running in singleton mode add the other members.
	if !p.Singleton {
		members = append(members, replSetMember{ID: 1, Host: clusterState.Sync.Addr})
		id := 2
		for _, peer := range clusterState.Async {
			members = append(members, replSetMember{ID: id, Host: peer.Addr})
			id++
		}
	}
	return replSetConfig{
		ID:      "rs0",
		Members: members,
	}
}

func (p *Process) reconfigure(config *state.Config) error {
	logger := p.Logger.New("fn", "reconfigure")

	if err := func() error {
		if config != nil && config.Role == state.RoleNone {
			logger.Info("nothing to do", "reason", "null role")
			return nil
		}

		// If we've already applied the same config, we don't need to do anything
		if p.configApplied && config != nil && p.config() != nil && config.Equal(p.config()) {
			logger.Info("nothing to do", "reason", "config already applied")
			return nil
		}

		if config != nil && config.Role == state.RolePrimary && p.running() {
			logger.Info("updating replica set configuration")
			replSetNew := p.replSetConfigFromState(config.State)
			err := p.setReplConfig(replSetNew)
			if err != nil {
				return err
			}
		}

		// TODO(jpg): We never need to reconfigure if we are are just a secondary.
		// If we're already running and it's just a change from async to sync with the same node, we don't need to restart
		if p.configApplied && p.running() && p.config() != nil && config != nil &&
			p.config().Role == state.RoleAsync && config.Role == state.RoleSync && config.Upstream.Meta["MONGODB_ID"] == p.config().Upstream.Meta["MONGODB_ID"] {
			logger.Info("nothing to do", "reason", "becoming sync with same upstream")
			return nil
		}

		// Make sure that we don't keep waiting for replication sync while reconfiguring
		p.cancelSyncWait()
		p.syncedDownstreamValue.Store((*discoverd.Instance)(nil))

		//TODO(jpg): We don't need to reconfigure on downstream change.
		// If we're already running and this is only a downstream change, just wait for the new downstream to catch up
		if p.running() && p.config().IsNewDownstream(config) {
			logger.Info("downstream changed", "to", config.Downstream.Addr)
			p.waitForSync(config.Downstream, false)
			return nil
		}

		if config == nil {
			config = p.config()
		}

		logger.Info("assuming primary, state nil?", "state_nil", config.State == nil)
		if config.Role == state.RolePrimary {
			return p.assumePrimary(config.Downstream, config.State)
		}

		return p.assumeStandby(config.Upstream, config.Downstream)
	}(); err != nil {
		return err
	}

	// Apply configuration.
	p.configValue.Store(config)
	p.configApplied = true

	return nil
}

func (p *Process) assumePrimary(downstream *discoverd.Instance, clusterState *state.State) (err error) {
	logger := p.Logger.New("fn", "assumePrimary")
	if downstream != nil {
		logger = logger.New("downstream", downstream.Addr)
	}

	if p.running() && p.config().Role == state.RoleSync {
		logger.Info("promoting to primary")
		p.waitForSync(downstream, true)
		return nil
	}

	logger.Info("starting as primary")

	// Assert that the process is not running. This should not occur.
	if p.running() {
		panic(fmt.Sprintf("unexpected state running role=%s", p.config().Role))
	}

	if err := p.writeConfig(configData{ReplicationEnabled: true /*SecurityEnabled: true*/}); err != nil {
		logger.Error("error writing config", "path", p.ConfigPath(), "err", err)
		return err
	}

	if err := p.start(); err != nil {
		return err
	}

	if err := p.initPrimaryDB(clusterState); err != nil {
		if e := p.stop(); err != nil {
			logger.Debug("ignoring error stopping process", "err", e)
		}
		return err
	}

	if downstream != nil {
		p.waitForSync(downstream, true)
	}

	return nil
}

func (p *Process) assumeStandby(upstream, downstream *discoverd.Instance) error {
	logger := p.Logger.New("fn", "assumeStandby", "upstream", upstream.Addr)
	logger.Info("starting up as standby")

	if err := p.writeConfig(configData{ReplicationEnabled: true}); err != nil {
		logger.Error("error writing config", "path", p.ConfigPath(), "err", err)
		return err
	}

	/*var backupInfo *BackupInfo*/
	if p.running() {
		if err := p.stop(); err != nil {
			return err
		}
	} else {

	}

	if err := p.start(); err != nil {
		return err
	}
	if err := p.waitForUpstream(upstream); err != nil {
		return err
	}

	if downstream != nil {
		p.waitForSync(downstream, false)
	}

	return nil
}

func (p Process) isReplInitialised() (bool, error) {
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:   []string{"127.0.0.1:" + p.Port},
		Direct:  true,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return false, err
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)
	result := bson.M{}
	if err := session.Run(bson.D{{"replSetGetStatus", 1}}, &result); err != nil {
		if merr, ok := err.(*mgo.QueryError); ok && merr.Code == 94 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p Process) isUserCreated() (bool, error) {
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:   []string{"127.0.0.1:" + p.Port},
		Direct:  true,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return false, err
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)

	n, err := session.DB("system").C("users").Find("flynn").Count()
	if err != nil {
		if merr, ok := err.(*mgo.QueryError); ok && merr.Code == 13 {
			return false, nil
		}
		return false, err
	}
	return n > 0, nil
}

func (p Process) createUser() error {
	// create a new session
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:   []string{"127.0.0.1:" + p.Port},
		Direct:  true,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)
	// TODO(jpg): Do we need to do anything further to test for success?
	var userResponse bson.M
	return session.DB("admin").Run(bson.D{
		{"createUser", "flynn"},
		{"pwd", p.Password},
		{"roles", []bson.M{{"role": "root", "db": "admin"}}},
	}, &userResponse)
}

// initPrimaryDB initializes the local database with the correct users and plugins.
func (p *Process) initPrimaryDB(clusterState *state.State) error {
	logger := p.Logger.New("fn", "initPrimaryDB")
	logger.Info("initializing primary database")

	mgo.SetDebug(true) // TEMP(benbjohnson)

	// check if admin user has been created
	//created, err := p.isUserCreated()
	//if err != nil {
	//	return err
	//}
	// user doesn't exist yet
	/*
		if !created {
			p.stop()
			// we need to start the database with both replication and security disabled
			if err := p.writeConfig(configData{}); err != nil {
				logger.Error("error writing config", "path", p.ConfigPath(), "err", err)
				return err
			}
			p.start()
			if err := p.createUser(); err != nil {
				return err
			}
			p.stop()

			if err := p.writeConfig(configData{ReplicationEnabled: true SecurityEnabled: true}); err != nil {
				logger.Error("error writing config", "path", p.ConfigPath(), "err", err)
				return err
			}
			p.start()
		}
	*/
	// check if replica set has been initialised
	initialized, err := p.isReplInitialised()
	if err != nil {
		return err
	}
	if !initialized && clusterState != nil {
		if err := p.replSetInitiate(clusterState); err != nil {
			return err
		}
	}

	// TODO(jpg): restart the database with new configuration, enabling authentication
	return nil
}

func (p *Process) replSetInitiate(clusterState *state.State) error {
	logger := p.Logger.New("fn", "replSetInitiate")
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:   []string{"127.0.0.1:" + p.Port},
		Direct:  true,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	defer session.Close()

	session.SetMode(mgo.Monotonic, true)

	for {
		var initiateResponse bson.M
		err := session.Run(bson.M{
			"replSetInitiate": p.replSetConfigFromState(clusterState),
		}, &initiateResponse)
		if merr, ok := err.(*mgo.QueryError); ok && merr.Code == 74 {
			logger.Info("not all peers present yet, waiting 5 seconds")
			time.Sleep(5 * time.Second)
			continue
		}
		if err != nil {
			return err
		}
		return nil
	}
}

// upstreamTimeout is of the order of the discoverd heartbeat to prevent
// waiting for an upstream which has gone down.
var upstreamTimeout = 10 * time.Second

func (p *Process) addr() string {
	return net.JoinHostPort(p.Host, p.Port)
}

func httpAddr(addr string) string {
	host, p, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(p)
	return fmt.Sprintf("%s:%d", host, port+1)
}

func (p *Process) waitForUpstream(upstream *discoverd.Instance) error {
	logger := p.Logger.New("fn", "waitForUpstream", "upstream", upstream.Addr, "upstream_http_addr", httpAddr(upstream.Addr))
	logger.Info("waiting for upstream to come online")
	upstreamClient := client.NewClient(upstream.Addr)

	timer := time.NewTimer(upstreamTimeout)
	defer timer.Stop()

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		status, err := upstreamClient.Status()
		if err == nil {
			logger.Info("status", "running", status.Database.Running, "xlog", status.Database.XLog, "user_exists", status.Database.UserExists)
		}
		if err != nil {
			logger.Error("error getting upstream status", "err", err)
		} else if status.Database.Running && status.Database.XLog != "" /*&& status.Database.UserExists*/ { // FIXME(benbjohnson)
			logger.Info("upstream is online")
			return nil
		}

		select {
		case <-timer.C:
			logger.Error("upstream did not come online in time")
			return errors.New("upstream is offline")
		case <-ticker.C:
		}
	}
}

func (p *Process) connectLocal() (*mgo.Session, error) {
	session, err := mgo.DialWithInfo(p.DialInfo())
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (p *Process) start() error {
	logger := p.Logger.New("fn", "start", "id", p.ID, "port", p.Port)
	logger.Info("starting process")

	cmd := NewCmd(exec.Command(filepath.Join(p.BinDir, "mongod"), "--config", p.ConfigPath()))
	if err := cmd.Start(); err != nil {
		logger.Error("failed to start process", "err", err)
		return err
	}
	p.cmd = cmd
	p.runningValue.Store(true)

	go func() {
		if <-cmd.Stopped(); cmd.Err() != nil {
			logger.Error("process unexpectedly exit", "err", cmd.Err())
			shutdown.ExitWithCode(1)
		}
	}()

	logger.Debug("waiting for process to start")

	timer := time.NewTimer(p.OpTimeout)
	defer timer.Stop()

	for {
		// Connect to server.
		// Retry after sleep if an error occurs.
		if err := func() error {
			session, err := mgo.DialWithInfo(&mgo.DialInfo{
				Addrs:   []string{"127.0.0.1:" + p.Port},
				Direct:  true,
				Timeout: p.OpTimeout,
			})
			if err != nil {
				return err
			}
			defer session.Close()

			return nil
		}(); err != nil {
			select {
			case <-timer.C:
				logger.Error("timed out waiting for process to start", "err", err)
				if err := p.stop(); err != nil {
					logger.Error("error stopping process", "err", err)
				}
				return err
			default:
				logger.Debug("ignoring error connecting to mongodb", "err", err)
				time.Sleep(checkInterval)
				continue
			}
		}

		logger.Debug("process started")
		return nil
	}
}

func (p *Process) stop() error {
	logger := p.Logger.New("fn", "stop")
	logger.Info("stopping mongodb")

	p.cancelSyncWait()

	// Attempt to kill.
	logger.Debug("stopping daemon")
	if err := p.cmd.Stop(); err != nil {
		logger.Error("error stopping command", "err", err)
	}

	// Wait for cmd to stop or timeout.
	select {
	case <-time.After(p.OpTimeout):
		return errors.New("unable to kill process")
	case <-p.cmd.Stopped():
		p.runningValue.Store(false)
		return nil
	}
}

func (p *Process) Info() (*client.DatabaseInfo, error) {
	info := &client.DatabaseInfo{
		Config:           p.config(),
		Running:          p.running(),
		SyncedDownstream: p.syncedDownstream(),
	}

	xlog, err := p.XLogPosition()
	info.XLog = string(xlog)
	if err != nil {
		return info, err
	}

	info.UserExists, err = p.userExists()
	if err != nil {
		return info, err
	}
	info.ReadWrite, err = p.isReadWrite()
	if err != nil {
		return info, err
	}
	return info, err
}

func (p *Process) isReadWrite() (bool, error) {
	if !p.running() {
		return false, nil
	}

	session, err := p.connectLocal()
	if err != nil {
		return false, err
	}
	defer session.Close()

	var entry struct {
		Retval struct {
			IsMaster bool `bson:"ismaster"`
		} `bson:"retval"`
	}
	if err := session.Run(bson.D{{"eval", `db.isMaster()`}}, &entry); err != nil {
		return false, err
	}
	return entry.Retval.IsMaster, nil
}

func (p *Process) userExists() (bool, error) {
	if !p.running() {
		return false, errors.New("mongod is not running")
	}

	session, err := p.connectLocal()
	if err != nil {
		return false, err
	}
	defer session.Close()

	var entry struct {
		Retval bson.M `bson:"retval"`
	}
	if err := session.Run(bson.M{"usersInfo": bson.M{"user": "flynn", "db": "admin"}}, &entry); err != nil {
		return false, err
	}
	return entry.Retval != nil, nil
}

func (p *Process) waitForSync(downstream *discoverd.Instance, enableWrites bool) {
	p.Logger.Debug("waiting for downstream sync")

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	var once sync.Once
	p.cancelSyncWait = func() {
		once.Do(func() { close(stopCh); <-doneCh })
	}

	go func() {
		defer close(doneCh)

		startTime := time.Now().UTC()
		logger := p.Logger.New(
			"fn", "waitForSync",
			"sync_name", downstream.Meta["MONGODB_ID"],
			"start_time", log15.Lazy{func() time.Time { return startTime }},
		)

		logger.Info("waiting for downstream replication to catch up")
		defer logger.Info("finished waiting for downstream replication")

		prevSlaveXLog := p.XLog().Zero()
		for {
			logger.Debug("checking downstream sync")

			// Check if "wait sync" has been canceled.
			select {
			case <-stopCh:
				logger.Debug("canceled, stopping")
				return
			default:
			}

			// Read local master status.
			masterXLog, err := p.XLogPosition()
			if err != nil {
				logger.Error("error reading master xlog", "err", err)
				startTime = time.Now().UTC()
				select {
				case <-stopCh:
					logger.Debug("canceled, stopping")
					return
				case <-time.After(checkInterval):
				}
				continue
			}
			logger.Info("master xlog", "gtid", masterXLog)

			// Read downstream slave status.
			slaveXLog, err := p.nodeXLogPosition(&mgo.DialInfo{
				Addrs:   []string{downstream.Addr},
				Timeout: p.OpTimeout,
			})
			if err != nil {
				logger.Error("error reading slave xlog", "err", err)
				startTime = time.Now().UTC()
				select {
				case <-stopCh:
					logger.Debug("canceled, stopping")
					return
				case <-time.After(checkInterval):
				}
				continue
			}

			logger.Info("mongodb slave xlog", "gtid", slaveXLog)

			elapsedTime := time.Since(startTime)
			logger := logger.New(
				"master_log_pos", masterXLog,
				"slave_log_pos", slaveXLog,
				"elapsed", elapsedTime,
			)

			// Mark downstream server as synced if the xlog matches the master.
			if cmp, err := p.XLog().Compare(masterXLog, slaveXLog); err == nil && cmp == 0 {
				logger.Info("downstream caught up")
				p.syncedDownstreamValue.Store(downstream)
				break
			}

			// If the slave's xlog is making progress then reset the start time.
			if cmp, err := p.XLog().Compare(prevSlaveXLog, slaveXLog); err == nil && cmp == -1 {
				logger.Debug("slave status progressing, resetting start time")
				startTime = time.Now().UTC()
			}
			prevSlaveXLog = slaveXLog

			if elapsedTime > p.ReplTimeout {
				logger.Error("error checking replication status", "err", "downstream unable to make forward progress")
				return
			}

			logger.Debug("continuing replication check")
			select {
			case <-stopCh:
				logger.Debug("canceled, stopping")
				return
			case <-time.After(checkInterval):
			}
		}
	}()
}

// DialInfo returns dial info for connecting to the local process as the "flynn" user.
func (p *Process) DialInfo() *mgo.DialInfo {
	return &mgo.DialInfo{
		Addrs:   []string{p.addr()},
		Timeout: p.OpTimeout,
	}
}

func (p *Process) XLogPosition() (xlog.Position, error) {
	return p.nodeXLogPosition(p.DialInfo())
}

// XLogPosition returns the current XLogPosition of node specified by DSN.
func (p *Process) nodeXLogPosition(info *mgo.DialInfo) (xlog.Position, error) {
	session, err := mgo.DialWithInfo(info)
	if err != nil {
		return p.XLog().Zero(), err
	}
	defer session.Close()

	var entry bson.M
	// TODO(jpg): Investigate if it's better to get this via the
	// replica set status of if this is prefferred.
	if err := session.DB("local").C("oplog.rs").Find(nil).Sort("-ts").One(&entry); err != nil {
		return p.XLog().Zero(), fmt.Errorf("find oplog.rs.ts error: %s", err)
	}
	return xlog.Position(strconv.FormatInt(int64(entry["ts"].(bson.MongoTimestamp)), 10)), nil
}

func (p *Process) runCmd(cmd *exec.Cmd) error {
	p.Logger.Debug("running command", "fn", "runCmd", "cmd", cmd.Args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (p *Process) writeConfig(d configData) error {
	d.ID = p.ID
	d.Port = p.Port
	d.DataDir = p.DataDir

	f, err := os.Create(p.ConfigPath())
	if err != nil {
		return err
	}
	defer f.Close()

	return configTemplate.Execute(f, d)
}

type configData struct {
	ID                 string
	Port               string
	DataDir            string
	SecurityEnabled    bool
	ReplicationEnabled bool
}

var configTemplate = template.Must(template.New("mongod.conf").Parse(`
storage:
  dbPath: {{.DataDir}}
  journal:
    enabled: true
  engine: wiredTiger

# systemLog:
#  destination: file
#  path: {{.DataDir}}/mongod.log
#  logAppend: true

net:
  port: {{.Port}}

{{if .SecurityEnabled}}
security:
  authorization: enabled
{{end}}

{{if .ReplicationEnabled}}
replication:
  replSetName: rs0
  enableMajorityReadConcern: true
{{end}}
`[1:]))