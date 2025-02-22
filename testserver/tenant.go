// Copyright 2020 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package testserver

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func (ts *testServerImpl) isTenant() bool {
	// ts.curTenantID is initialized to firstTenantID in system tenant servers.
	// An uninitialized ts.curTenantID indicates that this TestServer is a
	// tenant.
	return ts.curTenantID < firstTenantID
}

// NewTenantServer creates and returns a new SQL tenant pointed at the receiver,
// which acts as a KV server, and starts it.
// The SQL tenant is responsible for all SQL processing and does not store any
// physical KV pairs. It issues KV RPCs to the receiver. The idea is to be able
// to create multiple SQL tenants each with an exclusive keyspace accessed
// through the KV server. The proxy bool determines whether to spin up a
// (singleton) proxy instance to which to direct the returned server's `PGUrl`
// method.
//
// WARNING: This functionality is internal and experimental and subject to
// change. See cockroach mt start-sql --help.
// NOTE: To use this, a caller must first define an interface that includes
// NewTenantServer, and subsequently cast the TestServer obtained from
// NewTestServer to this interface. Refer to the tests for an example.
func (ts *testServerImpl) NewTenantServer(proxy bool) (TestServer, error) {
	if proxy && !ts.serverArgs.secure {
		return nil, fmt.Errorf("%s: proxy cannot be used with insecure mode", tenantserverMessagePrefix)
	}
	cockroachBinary := ts.cmdArgs[0]
	tenantID, err := func() (int, error) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		if ts.state != stateRunning {
			return 0, errors.New("TestServer must be running before NewTenantServer may be called")
		}
		if ts.isTenant() {
			return 0, errors.New("cannot call NewTenantServer on a tenant")
		}
		tenantID := ts.curTenantID
		ts.curTenantID++
		return tenantID, nil
	}()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}

	secureFlag := "--insecure"
	certsDir := filepath.Join(ts.baseDir, "certs")
	if ts.serverArgs.secure {
		secureFlag = "--certs-dir=" + certsDir
		certArgs := []string{
			secureFlag,
			"--ca-key=" + filepath.Join(certsDir, "ca.key"),
		}
		for _, args := range [][]string{
			// Create tenant client certificate.
			{"mt", "cert", "create-tenant-client", fmt.Sprint(tenantID)},
		} {
			if err := exec.Command(cockroachBinary, append(args, certArgs...)...).Run(); err != nil {
				return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
			}
		}
	}
	// Create a new tenant.
	if err := ts.WaitForInit(); err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}
	pgURL := ts.PGURL()
	if pgURL == nil {
		return nil, fmt.Errorf("%s: url not found", tenantserverMessagePrefix)
	}
	db, err := sql.Open("postgres", pgURL.String())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("SELECT crdb_internal.create_tenant(%d)", tenantID)); err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}

	// TODO(asubiotto): We should pass ":0" as the sql addr to push port
	//  selection to the cockroach mt start-sql command. However, that requires
	//  that the mt start-sql command supports --listening-url-file so that this
	//  test harness can subsequently read the postgres url. The current
	//  approach is to do our best to find a free port and use that.
	addr := func() (string, error) {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return "", fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
		}
		// Use localhost because of certificate validation issues otherwise
		// (something about IP SANs).
		addr := "localhost:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
		if err := l.Close(); err != nil {
			return "", fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
		}
		return addr, nil
	}
	sqlAddr, err := addr()
	if err != nil {
		return nil, err
	}

	proxyAddr, err := func() (string, error) {
		<-ts.pgURL.set

		ts.mu.Lock()
		defer ts.mu.Unlock()
		if ts.proxyAddr != "" {
			return ts.proxyAddr, nil
		}
		var err error
		ts.proxyAddr, err = addr()
		if err != nil {
			return "", err
		}

		args := []string{
			"mt",
			"start-proxy",
			"--listen-addr",
			ts.proxyAddr,
			"--routing-rule",
			sqlAddr,
			"--listen-cert",
			filepath.Join(certsDir, "node.crt"),
			"--listen-key",
			filepath.Join(certsDir, "node.key"),
			"--listen-metrics=:0",
			"--skip-verify",
		}
		cmd := exec.Command(cockroachBinary, args...)
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
		}
		if cmd.Process != nil {
			log.Printf("%s: process %d started: %s", tenantserverMessagePrefix, cmd.Process.Pid,
				strings.Join(args, " "))
		}
		ts.proxyProcess = cmd.Process

		return ts.proxyAddr, nil
	}()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}

	args := []string{
		cockroachBinary,
		"mt",
		"start-sql",
		secureFlag,
		"--logtostderr",
		fmt.Sprintf("--tenant-id=%d", tenantID),
		"--kv-addrs=" + pgURL.Host,
		"--sql-addr=" + sqlAddr,
		"--http-addr=:0",
	}

	tenant := &testServerImpl{
		serverArgs: ts.serverArgs,
		state:      stateNew,
		baseDir:    ts.baseDir,
		cmdArgs:    args,
		stdout:     filepath.Join(ts.baseDir, logsDirName, fmt.Sprintf("cockroach.tenant.%d.stdout", tenantID)),
		stderr:     filepath.Join(ts.baseDir, logsDirName, fmt.Sprintf("cockroach.tenant.%d.stderr", tenantID)),
		// TODO(asubiotto): Specify listeningURLFile once we support dynamic
		//  ports.
		listeningURLFile: "",
	}

	// Start the tenant.
	// Initialize direct connection to the tenant. We need to use `orig` instead of `pgurl` because if the test server
	// is using a root password, this password does not carry over to the tenant; client certs will, though.
	tenantURL := ts.pgURL.orig
	tenantURL.Host = sqlAddr
	tenant.pgURL.set = make(chan struct{})

	tenant.setPGURL(&tenantURL)
	if err := tenant.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}
	if err := tenant.WaitForInit(); err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}

	tenantDB, err := sql.Open("postgres", tenantURL.String())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
	}
	defer tenantDB.Close()

	rootPassword := ""
	if proxy {
		// The proxy does not do client certs, so always set a password if we use the proxy.
		rootPassword = "admin"
	}
	if pw := ts.serverArgs.rootPW; pw != "" {
		rootPassword = pw
	}

	if rootPassword != "" {
		// Allow root to login via password.
		if _, err := tenantDB.Exec(`ALTER USER $1 WITH PASSWORD $2`, "root", rootPassword); err != nil {
			return nil, fmt.Errorf("%s: %w", tenantserverMessagePrefix, err)
		}

		// NB: need the lock since *tenantURL is owned by `tenant`.
		tenant.mu.Lock()
		v := tenantURL.Query()
		if proxy {
			// If using proxy, point url at the proxy instead of at the tenant directly.
			tenantURL.Host = proxyAddr
			// Massage the query string. The proxy expects the magic cluster name 'prancing-pony'. We remove the client
			// certs since we won't be using them (and they don't work through the proxy anyway).
			v.Add("options", "--cluster=prancing-pony-2")
		}

		// Client certs should not be used; we're using password auth.
		v.Del("sslcert")
		v.Del("sslkey")
		tenantURL.RawQuery = v.Encode()
		tenantURL.User = url.UserPassword("root", rootPassword)
		tenant.mu.Unlock()
	}

	return tenant, nil
}
