package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"os/signal"
	"sync"
	"time"
)

const (
	UnknownError           = -1
	DnsNoSuchHost          = -2 // From net/dnsclient.go
	DnsUnrecognizedAddress = -3
	DnsServerMisbehaving   = -4
	DnsTooManyRedirects    = -5
)

type ResConf struct {
	Name     string
	Address  string
	Interval string
}

type Status struct {
	When       time.Time
	StatusCode int
}

type ResConfStatus struct {
	conf   *ResConf
	Status *Status // if > 0 then http.Response.StatusCode
}

func CheckStatus(c *ResConf) *Status {
	st := &Status{time.Now(), 0}
	resp, err := http.Get(c.Address)
	if err != nil {
		if dnserr, ok := err.(*net.DNSError); ok {
			switch dnserr.Err {
			case "no such host":
				st.StatusCode = DnsNoSuchHost
			case "unrecognized address":
				st.StatusCode = DnsUnrecognizedAddress
			case "server misbehaving":
				st.StatusCode = DnsServerMisbehaving
			case "too many redirects":
				st.StatusCode = DnsTooManyRedirects
			}
		}
		st.StatusCode = UnknownError
		log.Printf("Unknown error:  %s", err)
	} else {
		st.StatusCode = resp.StatusCode
	}
	return st
}

func worker(c chan *ResConf, ret chan *ResConfStatus) {
	for {
		conf := <-c
		status := CheckStatus(conf)
		ret <- &ResConfStatus{conf, status}
	}
}

type Config struct {
	Configs []*ResConf
}

func NewConfig() *Config {
	return &Config{make([]*ResConf, 0)}
}

func (c *Config) Add(ac *ResConf) {
	c.Configs = append(c.Configs, ac)
}

type EqCmp func(*ResConf) bool

func (c *Config) Remove(eq EqCmp) *ResConf {
	for i, el := range c.Configs {
		if eq(el) {
			c.Configs = append(c.Configs[:i], c.Configs[i+1:]...)
			return el
		}
	}
	return nil
}

func (c *Config) Save(filepath string) error {
	f, err := os.OpenFile(filepath, os.O_WRONLY|os.O_CREATE, os.ModePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return err
}

func LoadConfig(filePath string) (*Config, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	config := NewConfig()
	if err = json.Unmarshal(b, config); err != nil {
		return nil, err
	}
	return config, nil
}

type StatusChecker struct {
	config      *Config
	queue       chan *ResConf
	statuses    map[string]*Status
	m           *sync.Mutex
	statusMutex *sync.Mutex
}

func NewStatusChecker(c *Config) *StatusChecker {
	if c == nil {
		c = NewConfig()
	}
	return &StatusChecker{
		c,
		make(chan *ResConf, 200),
		make(map[string]*Status),
		&sync.Mutex{},
		&sync.Mutex{},
	}
}

func (s *StatusChecker) Add(cfg *ResConf) bool {
	s.m.Lock()
	s.statusMutex.Lock()
	defer s.m.Unlock()
	defer s.statusMutex.Unlock()
	s.config.Add(cfg)
	s.statuses[cfg.Address] = &Status{}
	log.Printf("Add %s (%s)", cfg.Name, cfg.Address)
	return true
}

func (s *StatusChecker) Remove(eq EqCmp) bool {
	s.m.Lock()
	s.statusMutex.Lock()
	defer s.m.Unlock()
	defer s.statusMutex.Unlock()
	el := s.config.Remove(eq)
	if el == nil {
		log.Printf("No element matching eq.")
		return false
	}
	delete(s.statuses, el.Address)
	log.Printf("Removed: %s (%s)", el.Name, el.Address)
	return true
}

func (s *StatusChecker) CloseNicely() {
	s.m.Lock()
	defer s.m.Unlock()
	if err := s.config.Save(*configFilePath); err != nil {
		log.Print(err)
	}
}

func (s *StatusChecker) report(acs chan *ResConfStatus) {
	for {
		status := <-acs
		s.statusMutex.Lock()
		if _, ok := s.statuses[status.conf.Address]; ok {
			s.statuses[status.conf.Address] = status.Status
		}
		s.statusMutex.Unlock()
	}
}

func (s *StatusChecker) Run(numWorkers int) {
	r := make(chan *ResConfStatus)
	go s.report(r)
	for i := 0; i < numWorkers; i++ {
		go worker(s.queue, r)
	}

	s.m.Lock()
	for _, ac := range s.config.Configs {
		s.statusMutex.Lock()
		s.statuses[ac.Address] = &Status{}
		s.statusMutex.Unlock()
		s.queue <- ac
	}
	s.m.Unlock()

	c := time.Tick(*interval)
	for range c {
		s.m.Lock()
		for _, ac := range s.config.Configs {
			s.queue <- ac
		}
		s.m.Unlock()
	}
}

///////////////////////////////////////////////////////////////////////////////
// An RPC server part.
///////////////////////////////////////////////////////////////////////////////

type AdminServer struct {
	sc *StatusChecker
}

type KeyType int

const (
	AddressKeyType (KeyType) = 1
	NameKeyType    (KeyType) = 2
)

type RemoveRequest struct {
	Key  string
	Type KeyType
}

func (a *AdminServer) Add(cfg *ResConf, status *int) error {
	a.sc.Add(cfg)
	return nil
}

func (a *AdminServer) Remove(args RemoveRequest, status *int) error {
	ok := false
	switch args.Type {
	case AddressKeyType:
		ok = a.sc.Remove(func(el *ResConf) bool { return args.Key == el.Address })
	case NameKeyType:
		ok = a.sc.Remove(func(el *ResConf) bool { return args.Key == el.Name })
	}
	if !ok {
		*status = 1
	}
	return nil
}

///////////////////////////////////////////////////////////////////////////////
// A HTML handler part.
///////////////////////////////////////////////////////////////////////////////

const statusTmplStr = `
<html><head><title>Status: OK %d of %d</title></head>
<style type="text/css">
table, th, td {
	border: 1px solid black;
}
</style>
<body>
<table>
<tr>
<td>Nazwa</td>
<td>Adres</td>
<td>Ostatnio sprawdzony</td>
<td>Status</td>
</tr>
{{ range . }}
<tr>
<td>{{.Name}}</td><td>{{.Address}}</td>
{{with .Status}}
<td>{{.When.Format "02-01-2006 15:04:05"}}</td>
<td>{{.StatusCode}}</td>
{{else}}
<td> - </td><td>0</td>
{{end}}
</tr>
{{ end }}
</table>
</body>
</html>
`

var statusTmpl = template.Must(template.New("statuspage").Parse(statusTmplStr))

type tmplHelper struct {
	Name    string
	Address string
	Status  *Status
}

func StartStatusHandler(sc *StatusChecker) {
	http.HandleFunc("/status", func(rw http.ResponseWriter, req *http.Request) {
		arr := make([]tmplHelper, 0)
		for _, c := range sc.config.Configs {
			arr = append(arr, tmplHelper{c.Name, c.Address, sc.statuses[c.Address]})
		}
		if err := statusTmpl.Execute(rw, arr); err != nil {
			log.Printf("Tmpl render: %s", err)
		}
	})
	go http.ListenAndServe("localhost:8080", nil)
}

///////////////////////////////////////////////////////////////////////////////
// main part.
///////////////////////////////////////////////////////////////////////////////

var (
	workers        = flag.Int("workers", 1, "How many worker threads to start.")
	configFilePath = flag.String("config", "", "Config file.")
	interval       = flag.Duration("interval", 60*time.Second, "How often check all pages.")

	mode = flag.String("mode", "server", "server|add|remove - add and remove send a command to server.")
	addr = flag.String("addr", "localhost:1234", "A server where rpc will be exposed.")

	sName = flag.String("sname", "", "A name for address.")
	sAddr = flag.String("saddr", "", "A resource address to check.")
)

func main() {
	flag.Parse()

	if *mode == "server" {
		configs := []*ResConf{
			&ResConf{"Google", "http://www.googssle.com", "5m"},
			&ResConf{"Google", "http://www.google.com", "5m"},
			&ResConf{"Wykop", "http://www.wykop.pl", "5m"},
		}

		var config *Config
		var err error
		if len(*configFilePath) > 0 {
			config, err = LoadConfig(*configFilePath)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Loaded config from: %s with %d addresses", *configFilePath, len(config.Configs))
		}
		sc := NewStatusChecker(config)
		admin := &AdminServer{sc}
		rpc.Register(admin)
		rpc.HandleHTTP()
		l, e := net.Listen("tcp", *addr)
		if e != nil {
			log.Fatal("listen error:", e)
		}
		go http.Serve(l, nil)

		http.HandleFunc("/status", func(rw http.ResponseWriter, req *http.Request) {
			arr := make([]tmplHelper, 0)
			for _, c := range sc.config.Configs {
				arr = append(arr, tmplHelper{c.Name, c.Address, sc.statuses[c.Address]})
			}
			if err := statusTmpl.Execute(rw, arr); err != nil {
				log.Printf("Tmpl render: %s", err)
			}
		})
		go http.ListenAndServe("localhost:8080", nil)

		if len(*configFilePath) == 0 {
			sc.config.Configs = configs
		}

		// Handle interruptions.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		go func() {
			for range c {
				// sig is a ^C, handle it
				log.Printf("Interrupt... please be patient.")
				if len(*configFilePath) > 0 {
					log.Printf("saving config\n")
					sc.CloseNicely()
				}
				os.Exit(0)
			}
		}()

		sc.Run(*workers)
	} else if *mode == "add" {
		client, err := rpc.DialHTTP("tcp", *addr)
		if err != nil {
			log.Fatal("dialing:", err)
		}
		// Synchronous call
		ac := &ResConf{*sName, *sAddr, ""}
		var reply int
		err = client.Call("AdminServer.Add", ac, &reply)
		if err != nil {
			log.Fatal("AdminServer error:", err)
		}
		fmt.Printf("AdminServer.Add: %d\n", reply)
	} else if *mode == "remove" {
		client, err := rpc.DialHTTP("tcp", *addr)
		if err != nil {
			log.Fatal("dialing:", err)
		}

		snl := len(*sName)
		sal := len(*sAddr)
		if snl*sal != 0 || snl+sal == 0 {
			log.Fatalf("For -mode remove one must specify exactly one of -sname, -saddr")
		}
		// Synchronous call
		var rr RemoveRequest
		if snl > 0 {
			rr.Key = *sName
			rr.Type = NameKeyType
		} else if sal > 0 {
			rr.Key = *sAddr
			rr.Type = AddressKeyType
		} else {
			log.Fatalf("Must specify -sname or -saddr")
		}
		var reply int
		err = client.Call("AdminServer.Remove", rr, &reply)
		if err != nil {
			log.Fatal("AdminServer error:", err)
		}
		fmt.Printf("AdminServer.Remove: %d\n", reply)
	}
}
