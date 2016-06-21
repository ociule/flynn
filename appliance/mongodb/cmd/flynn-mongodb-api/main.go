package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/julienschmidt/httprouter"
	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/mgo.v2"
	"github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/mgo.v2/bson"
	"github.com/flynn/flynn/discoverd/client"
	"github.com/flynn/flynn/pkg/httphelper"
	"github.com/flynn/flynn/pkg/random"
	"github.com/flynn/flynn/pkg/resource"
	"github.com/flynn/flynn/pkg/shutdown"
)

var serviceName = os.Getenv("FLYNN_MONGO")
var serviceHost string

func init() {
	if serviceName == "" {
		serviceName = "mongodb"
	}
	serviceHost = fmt.Sprintf("leader.%s.discoverd", serviceName)
}

func main() {
	defer shutdown.Exit()

	api := &API{}

	router := httprouter.New()
	router.POST("/databases", api.createDatabase)
	router.DELETE("/databases", api.dropDatabase)
	router.GET("/ping", api.ping)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port

	hb, err := discoverd.AddServiceAndRegister(serviceName+"-api", addr)
	if err != nil {
		shutdown.Fatal(err)
	}
	shutdown.BeforeExit(func() { hb.Close() })

	handler := httphelper.ContextInjector(serviceName+"-api", httphelper.NewRequestLogger(router))
	shutdown.Fatal(http.ListenAndServe(addr, handler))
}

type API struct{}

func (a *API) createDatabase(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	username, password, database := random.Hex(16), random.Hex(16), random.Hex(16)

	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{net.JoinHostPort(serviceHost, "27017")},
		Username: "flynn",
		Password: os.Getenv("MONGO_PWD"),
		Database: "admin",
	})
	if err != nil {
		httphelper.Error(w, err)
		return
	}
	defer session.Close()

	// Create a user
	var userResponse bson.M
	if err := session.DB("admin").Run(bson.D{
		{"createUser", username},
		{"pwd", password},
		{"roles", []bson.M{
			{"role": "dbOwner", "db": database},
		}},
	}, &userResponse); err != nil {
		httphelper.Error(w, err)
		return
	}

	// Assign any required roles
	url := fmt.Sprintf("mongo://%s:%s@%s:27017/%s", username, password, serviceHost, database)
	httphelper.JSON(w, 200, resource.Resource{
		ID: fmt.Sprintf("/databases/%s:%s", username, database),
		Env: map[string]string{
			"FLYNN_MONGO":    serviceName,
			"MONGO_HOST":     serviceHost,
			"MONGO_USER":     username,
			"MONGO_PWD":      password,
			"MONGO_DATABASE": database,
			"DATABASE_URL":   url,
		},
	})
}

func (a *API) dropDatabase(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	id := strings.SplitN(strings.TrimPrefix(req.FormValue("id"), "/databases/"), ":", 2)
	if len(id) != 2 || id[1] == "" {
		httphelper.ValidationError(w, "id", "is invalid")
		return
	}
	user, database := id[0], id[1]

	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{net.JoinHostPort(serviceHost, "27017")},
		Username: "flynn",
		Password: os.Getenv("MONGO_PWD"),
		Database: database,
	})
	if err != nil {
		httphelper.Error(w, err)
		return
	}
	defer session.Close()

	// Delete user.
	if err := session.DB(database).Run(bson.D{{"dropUser", user}}, nil); err != nil {
		httphelper.Error(w, err)
		return
	}

	// Delete database.
	if err := session.DB(database).Run(bson.D{{"dropDatabase", 1}}, nil); err != nil {
		httphelper.Error(w, err)
		return
	}

	w.WriteHeader(200)
}

func (a *API) ping(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{net.JoinHostPort(serviceHost, "27017")},
		Username: "flynn",
		Password: os.Getenv("MONGO_PWD"),
		Database: "admin",
	})
	if err != nil {
		httphelper.Error(w, err)
		return
	}
	defer session.Close()

	w.WriteHeader(200)
}
