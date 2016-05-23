// Copyright (C) 2015 Nippon Telegraph and Telephone Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	log "github.com/Sirupsen/logrus"
	"github.com/Sirupsen/logrus/hooks/syslog"
	"github.com/jessevdk/go-flags"
	bgpconf "github.com/osrg/gobgp/config"
	bgpserver "github.com/osrg/gobgp/server"
	"github.com/osrg/goplane/config"
	"github.com/osrg/goplane/netlink"
	"io/ioutil"
	"log/syslog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

type Dataplaner interface {
	Serve() error
	AddVirtualNetwork(config.VirtualNetwork) error
	DeleteVirtualNetwork(config.VirtualNetwork) error
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)

	var opts struct {
		ConfigFile    string `short:"f" long:"config-file" description:"specifying a config file"`
		ConfigType    string `short:"t" long:"config-type" description:"specifying config type (toml, yaml, json)" default:"toml"`
		LogLevel      string `short:"l" long:"log-level" description:"specifying log level"`
		LogPlain      bool   `short:"p" long:"log-plain" description:"use plain format for logging (json by default)"`
		UseSyslog     string `short:"s" long:"syslog" description:"use syslogd"`
		Facility      string `long:"syslog-facility" description:"specify syslog facility"`
		DisableStdlog bool   `long:"disable-stdlog" description:"disable standard logging"`
		GrpcHost      string `long:"grpc-host" description:"grpc host" default:":50051"`
	}
	_, err := flags.Parse(&opts)
	if err != nil {
		os.Exit(1)
	}

	switch opts.LogLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}

	if opts.DisableStdlog == true {
		log.SetOutput(ioutil.Discard)
	} else {
		log.SetOutput(os.Stdout)
	}

	if opts.UseSyslog != "" {
		dst := strings.SplitN(opts.UseSyslog, ":", 2)
		network := ""
		addr := ""
		if len(dst) == 2 {
			network = dst[0]
			addr = dst[1]
		}

		facility := syslog.Priority(0)
		switch opts.Facility {
		case "kern":
			facility = syslog.LOG_KERN
		case "user":
			facility = syslog.LOG_USER
		case "mail":
			facility = syslog.LOG_MAIL
		case "daemon":
			facility = syslog.LOG_DAEMON
		case "auth":
			facility = syslog.LOG_AUTH
		case "syslog":
			facility = syslog.LOG_SYSLOG
		case "lpr":
			facility = syslog.LOG_LPR
		case "news":
			facility = syslog.LOG_NEWS
		case "uucp":
			facility = syslog.LOG_UUCP
		case "cron":
			facility = syslog.LOG_CRON
		case "authpriv":
			facility = syslog.LOG_AUTHPRIV
		case "ftp":
			facility = syslog.LOG_FTP
		case "local0":
			facility = syslog.LOG_LOCAL0
		case "local1":
			facility = syslog.LOG_LOCAL1
		case "local2":
			facility = syslog.LOG_LOCAL2
		case "local3":
			facility = syslog.LOG_LOCAL3
		case "local4":
			facility = syslog.LOG_LOCAL4
		case "local5":
			facility = syslog.LOG_LOCAL5
		case "local6":
			facility = syslog.LOG_LOCAL6
		case "local7":
			facility = syslog.LOG_LOCAL7
		}

		hook, err := logrus_syslog.NewSyslogHook(network, addr, syslog.LOG_INFO|facility, "bgpd")
		if err != nil {
			log.Error("Unable to connect to syslog daemon, ", opts.UseSyslog)
			os.Exit(1)
		} else {
			log.AddHook(hook)
		}
	}

	if opts.LogPlain == false {
		log.SetFormatter(&log.JSONFormatter{})
	}

	if opts.ConfigFile == "" {
		opts.ConfigFile = "goplaned.conf"
	}

	configCh := make(chan *config.Config)
	reloadCh := make(chan bool)
	go config.ReadConfigfileServe(opts.ConfigFile, opts.ConfigType, configCh, reloadCh)
	reloadCh <- true
	bgpServer := bgpserver.NewBgpServer()
	go bgpServer.Serve()

	// start grpc Server
	grpcServer := bgpserver.NewGrpcServer(opts.GrpcHost, bgpServer.GrpcReqCh)
	go grpcServer.Serve()

	var dataplane Dataplaner
	var c *config.Config
	var d *config.Dataplane

	for {
		select {
		case newConfig := <-configCh:
			var added, deleted, updated []bgpconf.Neighbor
			var updatePolicy bool

			if c == nil {
				c = newConfig
				if err := bgpServer.SetGlobalType(newConfig.Global); err != nil {
					log.Fatalf("failed to set global config: %s", err)
				}
				if err := bgpServer.SetRpkiConfig(newConfig.RpkiServers); err != nil {
					log.Fatalf("failed to set rpki config: %s", err)
				}
				if err := bgpServer.SetBmpConfig(newConfig.BmpServers); err != nil {
					log.Fatalf("failed to set bmp config: %s", err)
				}
				if err := bgpServer.SetMrtConfig(newConfig.MrtDump); err != nil {
					log.Fatalf("failed to set mrt config: %s", err)
				}
				p := config.ConfigToRoutingPolicy(newConfig)
				if err := bgpServer.SetRoutingPolicy(*p); err != nil {
					log.Fatalf("failed to set routing policy: %s", err)
				}

				added = newConfig.Neighbors
			} else {
				added, deleted, updated, updatePolicy = bgpconf.UpdateConfig(config.ConfigToBgpConfigSet(c), config.ConfigToBgpConfigSet(newConfig))
				if updatePolicy {
					log.Info("Policy config is updated")
					p := bgpconf.ConfigSetToRoutingPolicy(config.ConfigToBgpConfigSet(newConfig))
					bgpServer.UpdatePolicy(*p)
				}
				c = newConfig
			}

			for _, p := range added {
				log.Infof("Peer %v is added", p.Config.NeighborAddress)
				bgpServer.PeerAdd(p)
			}
			for _, p := range deleted {
				log.Infof("Peer %v is deleted", p.Config.NeighborAddress)
				bgpServer.PeerDelete(p)
			}
			for _, p := range updated {
				log.Infof("Peer %v is updated", p.Config.NeighborAddress)
				u, _ := bgpServer.PeerUpdate(p)
				updatePolicy = updatePolicy || u
			}

			if updatePolicy {
				// TODO: we want to apply the new policies to the existing
				// routes here. Sending SOFT_RESET_IN to all the peers works
				// for the change of in and import policies. SOFT_RESET_OUT is
				// necessary for the export policy but we can't blindly
				// execute SOFT_RESET_OUT because we unnecessarily advertize
				// the existing routes. Needs to investigate the changes of
				// policies and handle only affected peers.
				ch := make(chan *bgpserver.GrpcResponse)
				bgpServer.GrpcReqCh <- &bgpserver.GrpcRequest{
					RequestType: bgpserver.REQ_NEIGHBOR_SOFT_RESET_IN,
					Name:        "all",
					ResponseCh:  ch,
				}
				<-ch
			}

			if dataplane == nil {
				switch newConfig.Dataplane.Type {
				case "netlink":
					log.Debug("new dataplane: netlink")
					dataplane = netlink.NewDataplane(newConfig, bgpServer.GrpcReqCh)
					go func() {
						err := dataplane.Serve()
						if err != nil {
							log.Errorf("dataplane finished with err: %s", err)
						}
					}()
				default:
					log.Errorf("Invalid dataplane type(%s). dataplane engine can't be started", newConfig.Dataplane.Type)
				}
			}

			as, ds := config.UpdateConfig(d, newConfig.Dataplane)
			d = &newConfig.Dataplane

			for _, v := range as {
				log.Infof("VirtualNetwork %s is added", v.RD)
				dataplane.AddVirtualNetwork(v)
			}
			for _, v := range ds {
				log.Infof("VirtualNetwork %s is deleted", v.RD)
				dataplane.DeleteVirtualNetwork(v)
			}

		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				log.Info("reload the config file")
				reloadCh <- true
			}
		}
	}
}
