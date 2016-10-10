package main

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/nhooyr/log"
)

// TODO custom config file
type proxy struct {
	BindInterfaces []string `json:"bindInterfaces"`
	Email          string   `json:"email"`
	CacheDir       string   `json:"cacheDir"`
	Protos         []struct {
		Name  string            `json:"name"`
		Hosts map[string]string `json:"hosts"`
	} `json:"protos"`
	DefaultProto string `json:"defaultProto"`

	// Map of protocol names to hostnames to backends.
	backends map[string]map[string]*backend
	manager  autocert.Manager
	config   *tls.Config
}

func (p *proxy) init() error {
	if len(p.BindInterfaces) == 0 {
		p.BindInterfaces = []string{""}
	}
	if p.DefaultProto == "" {
		return errors.New("defaultProto is empty or missing")
	}
	p.config = &tls.Config{
		GetCertificate:           p.manager.GetCertificate,
		PreferServerCipherSuites: true, // See golang/go#12895 for why.
		MinVersion:               tls.VersionTLS12,
	}
	p.backends = make(map[string]map[string]*backend)
	var hosts []string
	for i, proto := range p.Protos {
		if proto.Name == "" {
			return fmt.Errorf("protos[%d].name is empty or missing", i)
		}
		if len(proto.Hosts) == 0 {
			return fmt.Errorf("protos[%d].hosts is empty or missing", i)
		}
		p.backends[proto.Name] = make(map[string]*backend)
		for host, addr := range proto.Hosts {
			if host == "" {
				return fmt.Errorf("empty key in protos[%d].hosts", i)
			} else if addr == "" {
				return fmt.Errorf("protos[%d].hosts.%q is empty", i, host)
			}
			p.backends[proto.Name][host] = &backend{
				fmt.Sprintf("%q.%q: ", proto.Name, host),
				addr,
			}
			if !contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		p.config.NextProtos = append(p.config.NextProtos, proto.Name)
	}
	var ok bool
	p.backends[""], ok = p.backends[p.DefaultProto]
	if !ok {
		return fmt.Errorf("defaultProto (%q) is not defined in protos", p.DefaultProto)
	}

	if p.CacheDir == "" {
		return errors.New("empty or missing cacheDir")
	}
	p.manager = autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(hosts...),
		Cache:      autocert.DirCache(p.CacheDir),
		Email:      p.Email,
		Client: &acme.Client{
			HTTPClient: &http.Client{
				Timeout: 15 * time.Second,
			},
		},
	}

	keys := make([][32]byte, 1, 96)
	_, err := rand.Read(keys[0][:])
	if err != nil {
		return fmt.Errorf("session ticket key generation failed: %v", err)
	}
	p.config.SetSessionTicketKeys(keys)
	go p.rotateSessionTicketKeys(keys)
	return nil
}

func contains(strs []string, s1 string) bool {
	for _, s2 := range strs {
		if s1 == s2 {
			return true
		}
	}
	return false
}

func (p *proxy) rotateSessionTicketKeys(keys [][32]byte) {
	for {
		time.Sleep(1 * time.Hour)
		if len(keys) < cap(keys) {
			keys = keys[:len(keys)+1]
		}
		for s1, s2 := len(keys)-2, len(keys)-1; s2 > 0; s1, s2 = s1-1, s2-1 {
			keys[s2] = keys[s1]
		}
		_, err := rand.Read(keys[0][:])
		if err != nil {
			log.Fatalf("error generating session ticket key: %v", err)
		}
		p.config.SetSessionTicketKeys(keys)
	}
}

func (p *proxy) serve(l net.Listener) error {
	defer l.Close()
	var delay time.Duration
	for {
		c, err := l.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
					if delay > time.Second {
						delay = time.Second
					}
				}
				log.Printf("%v; retrying in %v", err, delay)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		go p.handle(c)
	}
}

func (p *proxy) handle(c net.Conn) {
	tlc := tls.Server(c, p.config)
	err := tlc.Handshake()
	if err != nil {
		log.Printf("TLS handshake error from %v: %v", c.RemoteAddr(), err)
		_ = c.Close()
		return
	}
	cs := tlc.ConnectionState()
	// Protocol is guaranteed to exist.
	hosts := p.backends[cs.NegotiatedProtocol]
	b, ok := hosts[cs.ServerName]
	if !ok {
		log.Printf("unable to find %q.%q for %v", cs.NegotiatedProtocol,
			cs.ServerName, c.RemoteAddr())
		_ = c.Close()
		return
	}
	b.handle(tlc)
}

type backend struct {
	name string
	addr string
}

var dialer = &net.Dialer{
	Timeout: 3 * time.Second,
	// No DualStack or KeepAlive because the dialer is used to
	// connect locally, not on the internet.
	// Thus there is no need to worry about broken IPv6
	// and KeepAlive is handled by incoming connection because
	// they are proxied.
}

var bufferPool = sync.Pool{
	New: func() interface{} {
		// TODO maybe different buffer size?
		// benchmark pls
		return make([]byte, 1<<15)
	},
}

func (b *backend) handle(c1 net.Conn) {
	b.logf("accepted %v", c1.RemoteAddr())
	c2, err := dialer.Dial("tcp", b.addr)
	if err != nil {
		b.log(err)
		_ = c1.Close()
		b.logf("disconnected %v", c1.RemoteAddr())
		return
	}
	first := make(chan<- struct{}, 1)
	cp := func(dst net.Conn, src net.Conn) {
		buf := bufferPool.Get().([]byte)
		// TODO use splice on linux
		// TODO needs some timeout to prevent torshammer ddos
		_, err := io.CopyBuffer(dst, src, buf)
		select {
		case first <- struct{}{}:
			if err != nil {
				b.log(err)
			}
			_ = dst.Close()
			_ = src.Close()
			b.logf("disconnected %v", c1.RemoteAddr())
		default:
		}
		bufferPool.Put(buf)
	}
	go cp(c1, c2)
	cp(c2, c1)
}

func (b *backend) logf(format string, v ...interface{}) {
	log.Printf(b.name+format, v...)
}

func (b *backend) log(err error) {
	log.Print(b.name, err)
}
