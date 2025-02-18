// Package cassandra implements the Driver interface.
package cassandra

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/db-journey/migrate/direction"
	"github.com/db-journey/migrate/driver"
	"github.com/db-journey/migrate/file"
	"github.com/gocql/gocql"
)

type Driver struct {
	session *gocql.Session
}

// make sure our driver still implements the driver.Driver interface
var _ driver.Driver = (*Driver)(nil)

const (
	tableName = "schema_migrations"
)

// Cassandra Driver URL format:
// cassandra://host:port/keyspace?protocol=version&consistency=level
//
// Examples:
// cassandra://localhost/SpaceOfKeys?protocol=4
// cassandra://localhost/SpaceOfKeys?protocol=4&consistency=all
// cassandra://localhost/SpaceOfKeys?consistency=quorum
func Open(rawurl string) (driver.Driver, error) {
	driver := &Driver{}
	u, err := url.Parse(rawurl)

	cluster := gocql.NewCluster(u.Host)
	cluster.Keyspace = u.Path[1:len(u.Path)]
	cluster.Consistency = gocql.All
	cluster.Timeout = 1 * time.Minute

	if consistencyStr := u.Query().Get("consistency"); len(consistencyStr) > 0 {
		// Warning: gocql.ParseConsistency will PANIC if there's an error.
		// See https://github.com/gocql/gocql/commit/f52d33ca51e4216a6bf6af74f80e023e69700afd
		cluster.Consistency = gocql.ParseConsistency(consistencyStr)
	}

	if len(u.Query().Get("protocol")) > 0 {
		protoversion, err := strconv.Atoi(u.Query().Get("protocol"))
		if err != nil {
			return nil, err
		}

		cluster.ProtoVersion = protoversion
	}

	if _, ok := u.Query()["disable_init_host_lookup"]; ok {
		cluster.DisableInitialHostLookup = true
	}

	// Check if url user struct is null
	if u.User != nil {
		password, passwordSet := u.User.Password()

		if passwordSet == false {
			return nil, fmt.Errorf("Missing password. Please provide password.")
		}

		cluster.Authenticator = gocql.PasswordAuthenticator{
			Username: u.User.Username(),
			Password: password,
		}

	}
	// handle ssl option
	if sslmode := u.Query().Get("sslmode"); sslmode != "" && sslmode != "disable" {
		cluster.SslOpts = &gocql.SslOptions{
			CaPath:                 u.Query().Get("sslrootcert"),
			CertPath:               u.Query().Get("sslcert"),
			KeyPath:                u.Query().Get("sslkey"),
			EnableHostVerification: sslmode == "verify-full",
		}
	}

	driver.session, err = cluster.CreateSession()
	if err != nil {
		return nil, err
	}

	if err := driver.ensureVersionTableExists(); err != nil {
		return nil, err
	}

	return driver, nil
}

func (driver *Driver) Close() error {
	driver.session.Close()
	return nil
}

func (driver *Driver) ensureVersionTableExists() error {
	err := driver.session.Query("CREATE TABLE IF NOT EXISTS " + tableName + " (version bigint primary key);").Exec()
	return err
}

func (driver *Driver) Migrate(f file.File) (err error) {
	defer func() {
		if err != nil {
			// Invert version direction if we couldn't apply the changes for some reason.
			if errRollback := driver.session.Query("DELETE FROM "+tableName+" WHERE version = ?", f.Version).Exec(); errRollback != nil {
				err = fmt.Errorf("%s; failed to rollback version: %s", err, errRollback)
			}
		}
	}()

	if err = f.ReadContent(); err != nil {
		return
	}

	if f.Direction == direction.Up {
		if err = driver.session.Query("INSERT INTO "+tableName+" (version) VALUES (?)", f.Version).Exec(); err != nil {
			return
		}
	} else if f.Direction == direction.Down {
		if err = driver.session.Query("DELETE FROM "+tableName+" WHERE version = ?", f.Version).Exec(); err != nil {
			return
		}
	}

	for _, query := range strings.Split(string(f.Content), ";") {
		query = strings.TrimSpace(query)
		if len(query) == 0 {
			continue
		}

		if err = driver.session.Query(query).Exec(); err != nil {
			return
		}
	}
	return
}

// Version returns the current migration version.
func (driver *Driver) Version() (file.Version, error) {
	versions, err := driver.Versions()
	if len(versions) == 0 {
		return 0, err
	}
	return versions[0], err
}

// Versions returns the list of applied migrations.
func (driver *Driver) Versions() (file.Versions, error) {
	versions := file.Versions{}
	iter := driver.session.Query("SELECT version FROM " + tableName).Iter()
	var version int64
	for iter.Scan(&version) {
		versions = append(versions, file.Version(version))
	}
	err := iter.Close()
	sort.Sort(sort.Reverse(versions))
	return versions, err
}

// Execute a SQL statement
func (driver *Driver) Execute(statement string) error {
	return driver.session.Query(statement).Exec()
}

func init() {
	driver.Register("cassandra", "cql", nil, Open)
}
