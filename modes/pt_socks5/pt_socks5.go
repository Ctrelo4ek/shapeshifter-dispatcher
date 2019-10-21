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
package pt_socks5

import (
	"fmt"
	options2 "github.com/OperatorFoundation/shapeshifter-dispatcher/common"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/pt_extras"
	"github.com/OperatorFoundation/shapeshifter-transports/transports/Dust"
	replicant "github.com/OperatorFoundation/shapeshifter-transports/transports/Replicant"
	"github.com/OperatorFoundation/shapeshifter-transports/transports/meeklite"
	"github.com/OperatorFoundation/shapeshifter-transports/transports/shadow"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"

	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/log"
	"github.com/OperatorFoundation/shapeshifter-dispatcher/common/socks5"
	"github.com/OperatorFoundation/shapeshifter-ipc"
	"github.com/OperatorFoundation/shapeshifter-transports/transports/obfs2"
	"github.com/OperatorFoundation/shapeshifter-transports/transports/obfs4"
)

func ClientSetup(socksAddr string, ptClientProxy *url.URL, names []string, options string) (launched bool, listeners []net.Listener) {
	// Launch each of the client listeners.
	for _, name := range names {
		ln, err := net.Listen("tcp", socksAddr)
		if err != nil {
			_ = pt.CmethodError(name, err.Error())
			continue
		}

		go clientAcceptLoop(name, ln, ptClientProxy, options)
		pt.Cmethod(name, socks5.Version(), ln.Addr())

		log.Infof("%s - registered listener: %s", name, ln.Addr())

		listeners = append(listeners, ln)
		launched = true
	}
	pt.CmethodsDone()

	return
}

func clientAcceptLoop(name string, ln net.Listener, proxyURI *url.URL, options string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				log.Errorf("serverAcceptLoop failed")
				_ = ln.Close()
				return
			}
			continue
		}
		go clientHandler(name, conn, proxyURI, options)
	}
}

func clientHandler(name string, conn net.Conn, proxyURI *url.URL, options string) {
	var needOptions = options == ""

	// Read the client's SOCKS handshake.
	socksReq, err := socks5.Handshake(conn, needOptions)
	if err != nil {
		log.Errorf("%s - client failed socks handshake: %s", name, err)
		return
	}
	addrStr := log.ElideAddr(socksReq.Target)

	//var args pt.Args
	//if needOptions {
	//	args = socksReq.Args
	//} else {
	//	args, err = pt.ParsePT2ClientParameters(options)
	//	if err != nil {
	//		return
	//	}
	//}

	args, argsErr := options2.ParseOptions(options)
	if argsErr != nil {
		log.Errorf("Error parsing transport options: %s", options)
		return
	}

	var dialer proxy.Dialer

	// Deal with arguments.
	transport, _ := pt_extras.ArgsToDialer(socksReq.Target, name, args, dialer)

	// Obtain the proxy dialer if any, and create the outgoing TCP connection.
	if proxyURI != nil {
		var proxyErr error
		dialer, proxyErr = proxy.FromURL(proxyURI, proxy.Direct)
		if proxyErr != nil {
			// This should basically never happen, since config protocol
			// verifies this.
			log.Errorf("%s(%s) - failed to obtain proxy dialer: %s", name, addrStr, log.ElideError(err))
			_ = socksReq.Reply(socks5.ReplyGeneralFailure)
			return
		}
	}

	fmt.Println("Got dialer", dialer, proxyURI, proxy.Direct)

	remote, err2 := transport.Dial()
	if err2 != nil {
		log.Errorf("%s(%s) - outgoing connection failed: %s", name, addrStr, log.ElideError(err))
		_ = socksReq.Reply(socks5.ErrorToReplyCode(err))
		return
	}
	err = socksReq.Reply(socks5.ReplySucceeded)
	if err != nil {
		log.Errorf("%s(%s) - SOCKS reply failed: %s", name, addrStr, log.ElideError(err))
		return
	}

	if err = copyLoop(conn, remote); err != nil {
		log.Warnf("%s(%s) - closed connection: %s", name, addrStr, log.ElideError(err))
	} else {
		log.Infof("%s(%s) - closed connection", name, addrStr)
	}

	return
}

func ServerSetup(ptServerInfo pt.ServerInfo, options string) (launched bool, listeners []net.Listener) {
	for _, bindaddr := range ptServerInfo.Bindaddrs {
		name := bindaddr.MethodName

		var listen func(address string) net.Listener

		args, argsErr := pt.ParsePT2ClientParameters(options)
		if argsErr != nil {
			log.Errorf("Error parsing transport options: %s", options)
			return
		}

		// Deal with arguments.
		switch name {
		case "obfs2":
			transport := obfs2.NewObfs2Transport()
			listen = transport.Listen
		case "obfs4":
			var dialer proxy.Dialer
			if cert, ok := args["cert"]; ok {
				if iatModeStr, ok2 := args["iat-mode"]; ok2 {
					iatMode, err := strconv.Atoi(iatModeStr[0])
					if err != nil {
						transport := obfs4.NewObfs4Client(cert[0], iatMode, dialer)
						listen = transport.Listen
					} else {
						log.Errorf("obfs4 transport bad iat-mode value: %s", iatModeStr)
						return
					}
				} else {
					log.Errorf("obfs4 transport missing cert argument: %s", args)
					return
				}
			} else {
				log.Errorf("obfs4 transport missing cert argument: %s", args)
				return
			}
		case "replicant":
			config, ok := args.Get("config")
			fmt.Println(config)
			if !ok {
				return false, nil
			}
			transport := replicant.New(replicant.Config{})
			listen = transport.Listen
		case "Dust":
			idPath, ok := args.Get("idPath")
			if !ok {
				return false, nil
			}
			transport := Dust.NewDustServer(idPath)
			listen = transport.Listen
		case "meeklite":
			Url, ok := args.Get("Url")
			if !ok {
				return false, nil
			}

			Front, ok2 := args.Get("Front")
			if !ok2 {
				return false, nil
			}
			transport := meeklite.NewMeekTransportWithFront(Url, Front)
			listen = transport.Listen

		case "shadow":
			password, ok := args.Get("password")
			if !ok {
				return false, nil
			}

			cipherName, ok2 := args.Get("cipherName")
			if !ok2 {
				return false, nil
			}

			transport := shadow.NewShadowServer(password, cipherName)
			listen = transport.Listen
		default:
			log.Errorf("Unknown transport: %s", name)
			return
		}

		f := listen

		transportLn := f(bindaddr.Addr.String())

		go serverAcceptLoop(name, transportLn, &ptServerInfo)

		// if args := f.Args(); args != nil {
		// 	pt.SmethodArgs(name, ln.Addr(), *args)
		// } else {
		// 	pt.SmethodArgs(name, ln.Addr(), nil)
		// }

		log.Infof("%s - registered listener: %s", name, log.ElideAddr(bindaddr.Addr.String()))

		listeners = append(listeners, transportLn)
		launched = true
	}
	pt.SmethodsDone()

	return
}

func serverAcceptLoop(name string, ln net.Listener, info *pt.ServerInfo) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if e, ok := err.(net.Error); ok && !e.Temporary() {
				return
			}
			continue
		}
		go serverHandler(name, conn, info)
	}
}

func serverHandler(name string, remote net.Conn, info *pt.ServerInfo) {

	addrStr := log.ElideAddr(remote.RemoteAddr().String())
	log.Infof("%s(%s) - new connection", name, addrStr)

	// Connect to the orport.
	orConn, err := pt.DialOr(info, remote.RemoteAddr().String(), name)
	if err != nil {
		log.Errorf("%s(%s) - failed to connect to ORPort: %s", name, addrStr, log.ElideError(err))
		return
	}

	if err = copyLoop(orConn, remote); err != nil {
		log.Warnf("%s(%s) - closed connection: %s", name, addrStr, log.ElideError(err))
	} else {
		log.Infof("%s(%s) - closed connection", name, addrStr)
	}

	return
}

func copyLoop(a net.Conn, b net.Conn) error {
	// Note: b is always the pt connection.  a is the SOCKS/ORPort connection.
	errChan := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err := io.Copy(b, a)
		errChan <- err
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(a, b)
		errChan <- err
	}()

	// Wait for both upstream and downstream to close.  Since one side
	// terminating closes the other, the second error in the channel will be
	// something like EINVAL (though io.Copy() will swallow EOF), so only the
	// first error is returned.
	wg.Wait()
	if len(errChan) > 0 {
		return <-errChan
	}

	return nil
}
