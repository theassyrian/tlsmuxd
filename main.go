package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	stdlog "log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/nhooyr/log"
	"github.com/xenolf/lego/acme"
)

func init() {
	acme.Logger = stdlog.New(ioutil.Discard, "", 0)
}

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("terminating")
		os.Exit(0)
	}()

	configDir := flag.String("c", "/usr/local/etc/tlsmuxd", "path to the configuration directory")
	flag.Parse()
	err := os.Chdir(*configDir)
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.Open("config.json")
	if err != nil {
		log.Fatal(err)
	}

	p := new(proxy)
	err = json.NewDecoder(f).Decode(&p)
	if err != nil {
		log.Fatal(err)
	}
	err = f.Close()
	if err != nil {
		log.Fatal(err)
	}
	err = p.init()
	if err != nil {
		log.Fatal(err)
	}

	for _, host := range p.BindInterfaces {
		l, err := net.Listen("tcp", net.JoinHostPort(host, "https"))
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			log.Fatal(p.serve(tcpKeepAliveListener{l.(*net.TCPListener)}))
		}()
	}

	log.Print("initialized")
	runtime.Goexit()
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(d.KeepAlive)
	return tc, nil
}
