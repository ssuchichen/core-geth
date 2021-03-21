// Copyright 2016 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/params"
)

const (
	ipcAPIs  = "admin:1.0 debug:1.0 eth:1.0 ethash:1.0 miner:1.0 net:1.0 personal:1.0 rpc:1.0 trace:1.0 txpool:1.0 web3:1.0"
	httpAPIs = "eth:1.0 net:1.0 rpc:1.0 web3:1.0"
)

// spawns geth with the given command line args, using a set of flags to minimise
// memory and disk IO. If the args don't set --datadir, the
// child g gets a temporary data directory.
func runMinimalGeth(t *testing.T, args ...string) *testgeth {
	// --ropsten to make the 'writing genesis to disk' faster (no accounts)
	// --networkid=1337 to avoid cache bump
	// --syncmode=full to avoid allocating fast sync bloom
	allArgs := []string{"--ropsten", "--networkid", "1337", "--syncmode=full", "--port", "0",
		"--nat", "none", "--nodiscover", "--maxpeers", "0", "--cache", "64"}
	return runGeth(t, append(allArgs, args...)...)
}

// TestConsoleCmdNetworkIdentities tests network identity variables at runtime for a geth instance.
// This provides a "production equivalent" integration test for consensus-relevant chain identity values which
// cannot be adequately unit tested because of reliance on cli context variables.
// These tests should cover expected default values and possible flag-interacting values, like --<chain> with --networkid=n.
func TestConsoleCmdNetworkIdentities(t *testing.T) {
	chainIdentityCases := []struct {
		flags       []string
		networkId   int
		chainId     int
		genesisHash string
	}{
		// Default chain value, without and with --networkid flag set.
		{[]string{}, 1, 1, params.MainnetGenesisHash.Hex()},
		{[]string{"--networkid", "42"}, 42, 1, params.MainnetGenesisHash.Hex()},

		// Non-default chain value, without and with --networkid flag set.
		{[]string{"--classic"}, 1, 61, params.MainnetGenesisHash.Hex()},
		{[]string{"--classic", "--networkid", "42"}, 42, 61, params.MainnetGenesisHash.Hex()},

		// All other possible --<chain> values.
		{[]string{"--mainnet"}, 1, 1, params.MainnetGenesisHash.Hex()},
		{[]string{"--testnet"}, 3, 3, params.RopstenGenesisHash.Hex()},
		{[]string{"--ropsten"}, 3, 3, params.RopstenGenesisHash.Hex()},
		{[]string{"--rinkeby"}, 4, 4, params.RinkebyGenesisHash.Hex()},
		{[]string{"--goerli"}, 5, 5, params.GoerliGenesisHash.Hex()},
		{[]string{"--kotti"}, 6, 6, params.KottiGenesisHash.Hex()},
		{[]string{"--mordor"}, 7, 63, params.MordorGenesisHash.Hex()},
		{[]string{"--yolov2"}, 133519467574834, 133519467574834, params.YoloV2GenesisHash.Hex()},
	}
	for i, p := range chainIdentityCases {

		// Disable networking, preventing false-negatives if in an environment without networking service
		// or collisions with an existing geth service.
		p.flags = append(p.flags, "--port", "0", "--maxpeers", "0", "--nodiscover", "--nat", "none")

		t.Run(fmt.Sprintf("%d/%v/networkid", i, p.flags),
			consoleCmdStdoutTest(p.flags, "admin.nodeInfo.protocols.eth.network", p.networkId))
		t.Run(fmt.Sprintf("%d/%v/chainid", i, p.flags),
			consoleCmdStdoutTest(p.flags, "admin.nodeInfo.protocols.eth.config.chainId", p.chainId))
		t.Run(fmt.Sprintf("%d/%v/genesis_hash", i, p.flags),
			consoleCmdStdoutTest(p.flags, "eth.getBlock(0, false).hash", strconv.Quote(p.genesisHash)))
	}
}

func consoleCmdStdoutTest(flags []string, execCmd string, want interface{}) func(t *testing.T) {
	return func(t *testing.T) {
		flags = append(flags, "--exec", execCmd, "console")
		geth := runGeth(t, flags...)
		geth.Expect(fmt.Sprintf(`%v
`, want))
		geth.ExpectExit()
		if status := geth.ExitStatus(); status != 0 {
			t.Errorf("expected exit status 0, got: %d", status)
		}
	}
}

// TestGethFailureToLaunch tests that geth fail immediately when given invalid run parameters (ie CLI args).
func TestGethFailureToLaunch(t *testing.T) {
	cases := []struct {
		flags            []string
		expectErrorReStr string
	}{
		{
			flags:            []string{"--badnet"},
			expectErrorReStr: "(?ism)incorrect usage.*",
		},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("TestIncorrectUsage: %v", c.flags), func(t *testing.T) {
			geth := runGeth(t, c.flags...)
			geth.ExpectRegexp(c.expectErrorReStr)
			geth.ExpectExit()
			if status := geth.ExitStatus(); status == 0 {
				t.Errorf("expected exit status != 0, got: %d", status)
			}
		})
	}
}

// TestGethStartupLogs tests that geth logs certain things (given some set of flags).
// In these cases, geth is run with a console command to print its name (and tests that it does).
func TestGethStartupLogs(t *testing.T) {
	// semiPersistentDatadir is used to house an adhoc datadir for co-dependent geth test cases.
	semiPersistentDatadir := filepath.Join(os.TempDir(), fmt.Sprintf("geth-startup-logs-test-%d", time.Now().Unix()))
	defer os.RemoveAll(semiPersistentDatadir)

	type matching struct {
		pattern string // pattern is the pattern to match against geth's stderr log.
		matches bool   // matches defines if the pattern should succeed or fail, ie. if the pattern should exist or should not exist.
	}
	cases := []struct {
		flags    []string
		matchers []matching

		// callback is run after the geth run case completes.
		// It can be used to reset any persistent state to provide a clean slate for the subsequent cases.
		callback func() error
	}{
		{
			// --<chain> flag is NOT given and datadir does not exist, representing a first tabula-rasa run.
			// Use without a --<chain> flag is deprecated. User will be warned.
			flags: []string{},
			matchers: []matching{
				{pattern: "(?ism).+WARN.+Not specifying a chain flag is deprecated.*", matches: true},
			},
		},
		{
			// Network flag is given.
			// --<chain> flag is NOT given. This is deprecated. User will be warned.
			// Same same but different as above.
			flags: []string{"--networkid=42"},
			matchers: []matching{
				{pattern: "(?ism).+WARN.+Not specifying a chain flag is deprecated.*", matches: true},
			},
		},
		// Little bit of a HACK.
		// This is a co-dependent sequence of two test cases.
		// First, startup a geth instance that will create a database, storing the genesis block.
		// This is a basic use case and has no errors.
		// The subsequent case then run geth re-using that datadir which has an existing chain database
		// and contains a stored genesis block.
		// Since the database contains a genesis block, the chain identity and config can (and will) be deduced from it;
		// this causes no need for a --<chain> CLI flag to be passed again. The user will not be warned of a missing --<chain> flag.
		{
			// --<chain> flag is given. All is well. Database (storing genesis) is initialized.
			flags: []string{"--datadir", semiPersistentDatadir, "--mainnet"},
			matchers: []matching{
				{pattern: "(?ism).*", matches: true},
			},
		},
		{
			// --<chain> flag is NOT given, BUT geth is being run on top of an existing
			// datadir. Geth will use the existing (stored) genesis found in it.
			// User should NOT be warned.
			flags: []string{"--datadir", semiPersistentDatadir},
			matchers: []matching{
				{pattern: "(?ism).+WARN.+Not specifying a chain flag is deprecated.*", matches: false},
				{pattern: "(?ism).+INFO.+Found stored genesis block.*", matches: true},
			},
			callback: func() error {
				// Clean up this mini-suite.
				return os.RemoveAll(semiPersistentDatadir)
			},
		},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("TestGethStartupLogs/%d: %v", i, c.flags), func(t *testing.T) {
			geth := runGeth(t, append(c.flags, "--exec", "admin.nodeInfo.name", "console")...)
			geth.ExpectRegexp("(?ism).*CoreGeth.*")
			geth.ExpectExit()
			if status := geth.ExitStatus(); status != 0 {
				t.Errorf("expected exit status == 0, got: %d", status)
			}
			for _, match := range c.matchers {
				if matched := regexp.MustCompile(match.pattern).MatchString(geth.StderrText()); matched != match.matches {
					t.Errorf("unexpected stderr output; want: %s (matching?=%v) got: %s", match.pattern, match.matches, geth.StderrText())
				}
			}
			if c.callback != nil {
				if err := c.callback(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Tests that a node embedded within a console can be started up properly and
// then terminated by closing the input stream.
func TestConsoleWelcome(t *testing.T) {
	coinbase := "0x8605cdbbdb6d264aa742e77020dcbc58fcdce182"

	// Start a geth console, make sure it's cleaned up and terminate the console
	geth := runMinimalGeth(t, "--etherbase", coinbase, "console")

	// Gather all the infos the welcome message needs to contain
	geth.SetTemplateFunc("clientname", func() string {
		if params.VersionName != "" {
			return params.VersionName
		}
		if geth.Name() != "" {
			return geth.Name()
		}
		return strings.Title(clientIdentifier)
	})
	geth.SetTemplateFunc("goos", func() string { return runtime.GOOS })
	geth.SetTemplateFunc("goarch", func() string { return runtime.GOARCH })
	geth.SetTemplateFunc("gover", runtime.Version)
	geth.SetTemplateFunc("gethver", func() string { return params.VersionWithCommit("", "") })
	geth.SetTemplateFunc("niltime", func() string {
		return time.Unix(0, 0).Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)")
	})
	geth.SetTemplateFunc("apis", func() string { return ipcAPIs })

	// Verify the actual welcome message to the required template
	geth.Expect(`
Welcome to the Geth JavaScript console!

instance: {{clientname}}/v{{gethver}}/{{goos}}-{{goarch}}/{{gover}}
coinbase: {{.Etherbase}}
at block: 0 ({{niltime}})
 datadir: {{.Datadir}}
 modules: {{apis}}

To exit, press ctrl-d
> {{.InputLine "exit"}}
`)
	geth.ExpectExit()
}

// Tests that a console can be attached to a running node via various means.
func TestAttachWelcome(t *testing.T) {
	var (
		ipc      string
		httpPort string
		wsPort   string
	)
	// Configure the instance for IPC attachment
	if runtime.GOOS == "windows" {
		ipc = `\\.\pipe\geth` + strconv.Itoa(trulyRandInt(100000, 999999))
	} else {
		ws := tmpdir(t)
		defer os.RemoveAll(ws)
		ipc = filepath.Join(ws, "geth.ipc")
	}
	// And HTTP + WS attachment
	p := trulyRandInt(1024, 65533) // Yeah, sometimes this will fail, sorry :P
	httpPort = strconv.Itoa(p)
	wsPort = strconv.Itoa(p + 1)
	geth := runMinimalGeth(t, "--etherbase", "0x8605cdbbdb6d264aa742e77020dcbc58fcdce182",
		"--ipcpath", ipc,
		"--http", "--http.port", httpPort,
		"--ws", "--ws.port", wsPort)
	t.Run("ipc", func(t *testing.T) {
		waitForEndpoint(t, ipc, 3*time.Second)
		testAttachWelcome(t, geth, "ipc:"+ipc, ipcAPIs)
	})
	t.Run("http", func(t *testing.T) {
		endpoint := "http://127.0.0.1:" + httpPort
		waitForEndpoint(t, endpoint, 3*time.Second)
		testAttachWelcome(t, geth, endpoint, httpAPIs)
	})
	t.Run("ws", func(t *testing.T) {
		endpoint := "ws://127.0.0.1:" + wsPort
		waitForEndpoint(t, endpoint, 3*time.Second)
		testAttachWelcome(t, geth, endpoint, httpAPIs)
	})
}

func testAttachWelcome(t *testing.T, geth *testgeth, endpoint, apis string) {
	// Attach to a running geth note and terminate immediately
	attach := runGeth(t, "attach", endpoint)
	defer attach.ExpectExit()
	attach.CloseStdin()

	// Gather all the infos the welcome message needs to contain
	attach.SetTemplateFunc("clientname", func() string {
		if params.VersionName != "" {
			return params.VersionName
		}
		if geth.Name() != "" {
			return geth.Name()
		}
		return strings.Title(clientIdentifier)
	})
	attach.SetTemplateFunc("goos", func() string { return runtime.GOOS })
	attach.SetTemplateFunc("goarch", func() string { return runtime.GOARCH })
	attach.SetTemplateFunc("gover", runtime.Version)
	attach.SetTemplateFunc("gethver", func() string { return params.VersionWithCommit("", "") })
	attach.SetTemplateFunc("etherbase", func() string { return geth.Etherbase })
	attach.SetTemplateFunc("niltime", func() string {
		return time.Unix(0, 0).Format("Mon Jan 02 2006 15:04:05 GMT-0700 (MST)")
	})
	attach.SetTemplateFunc("ipc", func() bool { return strings.HasPrefix(endpoint, "ipc") })
	attach.SetTemplateFunc("datadir", func() string { return geth.Datadir })
	attach.SetTemplateFunc("apis", func() string { return apis })

	// Verify the actual welcome message to the required template
	attach.Expect(`
Welcome to the Geth JavaScript console!

instance: {{clientname}}/v{{gethver}}/{{goos}}-{{goarch}}/{{gover}}
coinbase: {{etherbase}}
at block: 0 ({{niltime}}){{if ipc}}
 datadir: {{datadir}}{{end}}
 modules: {{apis}}

To exit, press ctrl-d
> {{.InputLine "exit" }}
`)
	attach.ExpectExit()
}

// trulyRandInt generates a crypto random integer used by the console tests to
// not clash network ports with other tests running cocurrently.
func trulyRandInt(lo, hi int) int {
	num, _ := rand.Int(rand.Reader, big.NewInt(int64(hi-lo)))
	return int(num.Int64()) + lo
}
