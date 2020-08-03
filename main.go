/*
 * Copyright (c) 2014-2015, Yawning Angel <yawning at torproject dot org>
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *    this list of conditions and the following disclaimer.
 *
 *  * Redistributions in binary form must reproduce the above copyright notice,
 *    this list of conditions and the following disclaimer in the documentation
 *    and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

// Go language Tor Pluggable Transport suite.  Works only as a managed
// client/server.
package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/kataras/golog"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/pt_extras"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/transports"
	"github.com/OperatorFoundation/shapeshifter-ipc/v2"

	"github.com/OperatorFoundation/shapeshifter-dispatcher/modes/pt_socks5"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/modes/stun_udp"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/modes/transparent_tcp"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/modes/transparent_udp"

	_ "github.com/OperatorFoundation/obfs4/proxy_dialers/proxy_http"
	_ "github.com/OperatorFoundation/obfs4/proxy_dialers/proxy_socks4"
)

const (
	dispatcherVersion = "0.0.7-dev"
	dispatcherLogFile = "dispatcher.log"
)

var stateDir string

func getVersion() string {
	return fmt.Sprintf("dispatcher-%s", dispatcherVersion)
}

const (
	socks5 = iota
	transparentTCP
	transparentUDP
	stunUDP
)

func main() {

	// Handle the command line arguments.
	_, execName := path.Split(os.Args[0])

	flag.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "shapeshifter-dispatcher is a PT v3.0 proxy supporting multiple transports and proxy modes\n\n")
		_, _ = fmt.Fprintf(os.Stderr, "Usage:\n\t%s -client -state [statedir] -version 2 -transports [transport1,transport2,...]\n\n", os.Args[0])
		_, _ = fmt.Fprintf(os.Stderr, "Example:\n\t%s -client -state state -version 2 -transports obfs2\n\n", os.Args[0])
		_, _ = fmt.Fprintf(os.Stderr, "Flags:\n\n")
		flag.PrintDefaults()
	}

	// Parsing flags starts here, but variables are not set to actual values until flag.Parse() is called.
	// PT 2.1 specification, 3.3.1.1. Common Configuration Parameters
	var ptversion = flag.String("ptversion", "2.1", "Specify the Pluggable Transport protocol version to use")

	statePath := flag.String("state", "state", "Specify the directory to use to store state information required by the transports")
	exitOnStdinClose := flag.Bool("exit-on-stdin-close", false, "Set to true to force the dispatcher to close when the stdin pipe is closed")

	transportsList := flag.String("transports", "", "Specify transports to enable")

	// PT 2.1 specification, 3.3.1.2. Pluggable PT Client Configuration Parameters
	proxy := flag.String("proxy", "", "Specify an HTTP or SOCKS4a proxy that the PT needs to use to reach the Internet")

	// PT 2.1 specification, 3.3.1.3. Pluggable PT Server Environment Variables
	options := flag.String("options", "", "Specify the transport options for the server")
	if *options != "" {
		println("--> -options flag found: ", *options)
	}

	bindAddr := flag.String("bindaddr", "", "Specify the bind address for transparent server")
	extorport := flag.String("extorport", "", "Specify the address of a server implementing the Extended OR Port protocol, which is used for per-connection metadata")
	authcookie := flag.String("authcookie", "", "Specify an authentication cookie, for use in authenticating with the Extended OR Port")

	// Experimental flags under consideration for PT 2.1
	socksAddr := flag.String("proxylistenaddr", "127.0.0.1:0", "Specify the bind address for the local SOCKS server provided by the client")
	optionsFile := flag.String("optionsFile", "", "store all the options in a single file")

	// Additional command line flags inherited from obfs4proxy
	showVer := flag.Bool("showVersion", false, "Print version and exit")
	logLevelStr := flag.String("logLevel", "ERROR", "Log level (ERROR/WARN/INFO/DEBUG)")
	enableLogging := flag.Bool("enableLogging", false, "Log to [state]/"+dispatcherLogFile)

	// Additional command line flags added to shapeshifter-dispatcher
	clientMode := flag.Bool("client", false, "Enable client mode")
	serverMode := flag.Bool("server", false, "Enable server mode")
	transparent := flag.Bool("transparent", false, "Enable transparent proxy mode. The default is protocol-aware proxy mode (socks5 for TCP, STUN for UDP)")
	udp := flag.Bool("udp", false, "Enable UDP proxy mode. The default is TCP proxy mode.")
	target := flag.String("target", "", "Specify transport server destination address")
	flag.Parse() // Flag variables are set to actual values here.

	// Start validation of command line arguments

	if *showVer {
		fmt.Printf("%s\n", getVersion())
		os.Exit(0)
	}

	logPath := path.Join(stateDir, dispatcherLogFile)
	logFile, logFileErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if logFileErr != nil {
		println("could open logFile")
	}

	golog.SetOutput(logFile)

	if enableLogging != nil {
		lowerCaseLogLevelStr := strings.ToLower(*logLevelStr)
		golog.SetLevel(lowerCaseLogLevelStr)

	} else {
		golog.SetLevel("fatal")
	}
	// Determine if this is a client or server, initialize the common state.
	launched := false
	isClient, err := checkIsClient(*clientMode, *serverMode)
	if err != nil {
		flag.Usage()
		golog.Fatalf("[ERROR]: %s - either --client or --server is required, or configure using PT 2.0 environment variables", execName)
	}
	if stateDir, err = makeStateDir(*statePath); err != nil {
		flag.Usage()
		golog.Fatalf("[ERROR]: %s - No state directory: Use --state", execName)
	}
	if *options != "" && *optionsFile != "" {
		golog.Fatal("cannot specify -options and -optionsFile at the same time")
	}
	if *optionsFile != "" {
		fmt.Println("checking for optionsFile")
		_, err := os.Stat(*optionsFile)
		if err != nil {
			golog.Errorf("optionsFile does not exist with error %s %s", *optionsFile, err.Error())
		} else {
			contents, readErr := ioutil.ReadFile(*optionsFile)
			if readErr != nil {
				golog.Errorf("could not open optionsFile: %s", *optionsFile)
			} else {
				*options = string(contents)
			}
		}
	}

	emptyString := ""
	validationError := validatetarget(isClient, &emptyString, &emptyString, target)
	if validationError != nil {
		golog.Error(validationError)
		return
	}

	mode := determineMode(*transparent, *udp)

	if isClient {
		if *target != "" {
			golog.Error("cannot use -target in client mode")
			return
		}
	} else {
		switch mode {
		case socks5:
			if *bindAddr == "" {
				golog.Errorf("-%s - socks5 mode requires a bindaddr", execName)
				return
			}
		case transparentTCP:
			if *bindAddr == "" {
				golog.Errorf("%s - transparent mode requires a bindaddr", execName)
				return
			}
		case transparentUDP:
			if *bindAddr == "" {
				golog.Errorf("%s - transparent mode requires a bindaddr", execName)
				return
			}
		case stunUDP:
			if *bindAddr == "" {
				golog.Errorf("%s - STUN mode requires a bindaddr", execName)
				return
			}
		default:
			golog.Errorf("unsupported mode %d", mode)
			return
		}
	}

	// Finished validation of command line arguments

	golog.Infof("%s - launched", getVersion())

	if isClient {
		golog.Infof("%s - initializing client transport listeners", execName)

		switch mode {
		case socks5:
			golog.Infof("%s - initializing client transport listeners", execName)
			ptClientProxy, names, nameErr := getClientNames(ptversion, transportsList, proxy)
			if nameErr != nil {
				golog.Errorf("must specify -version and -transports")
				return
			}
			launched = pt_socks5.ClientSetup(*socksAddr, ptClientProxy, names, *options)
		case transparentTCP:
			ptClientProxy, names, nameErr := getClientNames(ptversion, transportsList, proxy)
			if nameErr != nil {
				golog.Errorf("must specify -version and -transports")
				return
			}
			launched = transparent_tcp.ClientSetup(*socksAddr, ptClientProxy, names, *options)
		case transparentUDP:
			ptClientProxy, names, nameErr := getClientNames(ptversion, transportsList, proxy)
			if nameErr != nil {
				golog.Errorf("must specify -version and -transports")
				return
			}
			launched = transparent_udp.ClientSetup(*socksAddr, ptClientProxy, names, *options)
		case stunUDP:
			ptClientProxy, names, nameErr := getClientNames(ptversion, transportsList, proxy)
			if nameErr != nil {
				golog.Errorf("must specify -version and -transports")
				return
			}
			launched = stun_udp.ClientSetup(*socksAddr, ptClientProxy, names, *options)
		default:
			golog.Errorf("unsupported mode %d", mode)
		}
	} else {
		golog.Infof("initializing server transport listeners")

		switch mode {
		case socks5:
			golog.Infof("%s - initializing server transport listeners", execName)
			ptServerInfo := getServerInfo(bindAddr, options, transportsList, target, extorport, authcookie)
			launched = pt_socks5.ServerSetup(ptServerInfo, stateDir, *options)
		case transparentTCP:
			golog.Infof("%s - initializing server transport listeners", execName)
			ptServerInfo := getServerInfo(bindAddr, options, transportsList, target, extorport, authcookie)
			launched = transparent_tcp.ServerSetup(ptServerInfo, stateDir, *options)
		case transparentUDP:
			// launched = transparent_udp.ServerSetup(termMon, *bindAddr, *target)

			ptServerInfo := getServerInfo(bindAddr, options, transportsList, target, extorport, authcookie)
			launched = transparent_udp.ServerSetup(ptServerInfo, stateDir, *options)
		case stunUDP:
			ptServerInfo := getServerInfo(bindAddr, options, transportsList, target, extorport, authcookie)
			launched = stun_udp.ServerSetup(ptServerInfo, stateDir, *options)
		default:
			golog.Errorf("unsupported mode %d", mode)
		}
	}

	if !launched {
		// Initialization failed, the client or server setup routines should
		// have logged, so just exit here.
		os.Exit(-1)
	}

	golog.Infof("%s - accepting connections", execName)

	if *exitOnStdinClose {
		_, _ = io.Copy(ioutil.Discard, os.Stdin)
		os.Exit(-1)
	} else {
		select {}
	}
}

func determineMode(isTransparent bool, isUDP bool) int {
	if isTransparent && isUDP {
		golog.Infof("initializing transparent proxy")
		golog.Infof("initializing UDP transparent proxy")
		return transparentUDP
	} else if isTransparent {
		golog.Infof("initializing transparent proxy")
		golog.Infof("initializing TCP transparent proxy")
		return transparentTCP
	} else if isUDP {
		golog.Infof("initializing STUN UDP proxy")
		return stunUDP
	} else {
		golog.Infof("initializing PT 2.1 socks5 proxy")
		return socks5
	}
}

func checkIsClient(client bool, server bool) (bool, error) {
	if client {
		return true, nil
	} else if server {
		return false, nil
	} else {
		return true, nil
	}
}

func makeStateDir(statePath string) (string, error) {
	if statePath != "" {
		err := os.MkdirAll(statePath, 0700)
		return statePath, err
	}

	return pt.MakeStateDir()
}

func getClientNames(ptversion *string, transportsList *string, proxy *string) (clientProxy *url.URL, names []string, retErr error) {
	var ptClientInfo pt.ClientInfo
	var err error

	if ptversion == nil || transportsList == nil {
		golog.Infof("Falling back to environment variables for ptversion/transports.")
		ptClientInfo, err = pt.ClientSetup(transports.Transports())
		if err != nil {
			return nil, nil, err
		}
	} else {
		if *transportsList == "*" {
			ptClientInfo = pt.ClientInfo{MethodNames: transports.Transports()}
		} else {
			ptClientInfo = pt.ClientInfo{MethodNames: strings.Split(*transportsList, ",")}
		}
	}

	ptClientProxy, proxyErr := pt_extras.PtGetProxy(proxy)
	if proxyErr != nil {
		return nil, nil, proxyErr
	} else if ptClientProxy != nil {
		pt_extras.PtProxyDone()
	}

	return ptClientProxy, ptClientInfo.MethodNames, nil
}

func getServerInfo(bindaddrList *string, options *string, transportList *string, target *string, extorport *string, authcookie *string) pt.ServerInfo {
	var ptServerInfo pt.ServerInfo
	var err error
	var bindaddrs []pt.Bindaddr

	bindaddrs, err = getServerBindaddrs(bindaddrList, options, transportList)
	if err != nil {
		golog.Errorf(err.Error())
		golog.Errorf("Error parsing bindaddrs %q %q %q", *bindaddrList, *options, *transportList)
		return ptServerInfo
	}

	ptServerInfo = pt.ServerInfo{Bindaddrs: bindaddrs}
	ptServerInfo.OrAddr, err = pt.ResolveAddr(*target)
	if err != nil {
		golog.Errorf("Error resolving OR address %q %q", *target, err)
		return ptServerInfo
	}

	if *authcookie != "" {
		ptServerInfo.AuthCookiePath = *authcookie
	}

	if *extorport != "" {
		ptServerInfo.ExtendedOrAddr, err = pt.ResolveAddr(*extorport)
		if err != nil {
			golog.Errorf("Error resolving Extended OR address %q %q", *extorport, err)
			return ptServerInfo
		}
	}

	return ptServerInfo
}

// Return an array of Bindaddrs.
func getServerBindaddrs(bindaddrList *string, options *string, transports *string) ([]pt.Bindaddr, error) {
	var result []pt.Bindaddr
	var serverTransportOptions string
	var serverBindaddr string
	var serverTransports string
	var optionsMap map[string]map[string]interface{}
	var optionsError error

	// Parse the list of server transport options.
	if *options != "" {
		serverTransportOptions = *options
		if serverTransportOptions != "" {
			optionsMap, optionsError = pt.ParsePT2ServerParameters(serverTransportOptions)
			if optionsError != nil {
				golog.Errorf("Error parsing options map %q %q", serverTransportOptions, optionsError)
				return nil, fmt.Errorf("-options: %q: %s", serverTransportOptions, optionsError.Error())
			}
		}
	}

	// Get the list of all requested bindaddrs.
	if *bindaddrList != "" {
		serverBindaddr = *bindaddrList
	}

	for _, spec := range strings.Split(serverBindaddr, ",") {
		var bindaddr pt.Bindaddr

		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("-bindaddr: %q: doesn't contain \"-\"", spec)
		}
		bindaddr.MethodName = parts[0]
		addr, err := pt.ResolveAddr(parts[1])
		if err != nil {
			return nil, fmt.Errorf("-bindaddr: %q: %s", spec, err.Error())
		}
		bindaddr.Addr = addr
		bindaddr.Options = optionsMap[bindaddr.MethodName]
		result = append(result, bindaddr)
	}

	if transports == nil {
		return nil, errors.New("must specify -transport or -transports in server mode")
	} else {
		serverTransports = *transports
	}
	result = pt.FilterBindaddrs(result, strings.Split(serverTransports, ","))
	if len(result) == 0 {
		golog.Errorf("no valid bindaddrs")
	}
	return result, nil
}
