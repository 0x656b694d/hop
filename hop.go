package main

import (
    "bytes"
    "context"
    "crypto/tls"
    "crypto/x509"
    "golang.org/x/net/http2"
    "errors"
    "flag"
    "fmt"
    "io"
    "io/ioutil"
    "log"
    "math/rand"
    "net"
    "net/http"
    "net/http/httputil"
    "net/url"
    "os"
    "strconv"
    "strings"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

type hopHandler struct {}

var help = map[string]string {
    "-code:N": "responde with HTTP code N",
    "-crash": "stops the server without a response",
    "-fheader:H": "forward incoming header H to the following request",
    "-header:H=V": "add header H: V to the following request",
    "-help": "return help message",
    "-if:H=V": "execute next command if header H contains substring V",
    "-info": "return some info about the request",
    "-not": "reverts the effect of the next boolean command (if, on)",
    "-on:H": "executes next command if the server host name contains substring H",
    "-quit": "stops the server with a nice response",
    "-rheader:H=V": "add header H: V to the reponse",
    "-rnd:P": "execute next command with P% probability",
    "-rsize:B": "add B bytes of payload to the response",
    "-size:B": "add B bytes of payload to the following query",
    "-wait:T": "wait for T ms before response",
    "-env:V": "return the value of an environment variable",
}

var quit = make(chan int)

var transport *http.Transport
var tlsClientConfig *tls.Config
var tlsServerConfig *tls.Config

var http_proxy_url *url.URL
var https_proxy_url *url.URL

func proxy(req *http.Request) (*url.URL, error) {
    switch req.URL.Scheme {
    case "http": return http_proxy_url, nil
    case "https": return https_proxy_url, nil
    }
    return http.ProxyFromEnvironment(req)
}

func initTLS(cacert string) {
    log.Println("Initializing TLS")
    roots, err := x509.SystemCertPool()
    if err != nil {
        log.Fatalf("failed to obtain the system certificates pool: %s", err)
    }
    if len(cacert) != 0 {
        data, err := ioutil.ReadFile(cacert)
        if err != nil {
            log.Fatalf("failed to load root certificate from %s: %s", cacert, err)
        }
        ok := roots.AppendCertsFromPEM(data)
        if !ok {
            log.Fatalf("failed to parse root certificate from %s", cacert)
        }
    }
    tlsCert, err := tls.LoadX509KeyPair(certificate, key)
    if err != nil {
        log.Panicf("failed to load client certificate or key: %s", err)
    }
    tlsClientConfig = &tls.Config {
        RootCAs: roots,
        Certificates: []tls.Certificate { tlsCert },
        InsecureSkipVerify: true,
    }
    tlsServerConfig = &tls.Config {
        ClientCAs: roots,
    }
    if proxy_tunneling {
        transport = &http.Transport{
            Proxy:              proxy,
            MaxIdleConns:       10,
            IdleConnTimeout:    30 * time.Second,
            TLSClientConfig:    tlsClientConfig,
        }
    } else {
        transport = &http.Transport{
            Proxy:              nil,
            MaxIdleConns:       10,
            IdleConnTimeout:    30 * time.Second,
            TLSClientConfig:    tlsClientConfig,
        }
    }
    if err = http2.ConfigureTransport(transport); err != nil {
        log.Fatalf("Cannot configure http2 transport: %s", err)
    }
}

var http_client *http.Client
var https_client *http.Client

func buildRequest(url *url.URL, headers *map[string]string, size int) (*http.Request, error) {
    log.Printf("Call %s, sending %d bytes and %v", url, size, *headers)
    payload := bytes.Repeat([]byte{'X'}, size)

    req, err := http.NewRequest("GET", url.String(), bytes.NewReader(payload))
    if err != nil || req == nil {
        return nil, err
    }

    for h, v := range *headers {
        if strings.ToLower(h) == "host" {
            req.Host = v
        } else {
            req.Header.Set(h, v)
        }
    }
    return req, err
}

func callURL(req *http.Request) (*http.Response, error) {
    var client *http.Client
    var err error
    if req.URL.Scheme == "http" {
        if http_client == nil {
            http_client = &http.Client{}
        }
        client = http_client
        log.Println("Using HTTP client")
    } else if req.URL.Scheme == "https" {
        if transport == nil {
            return nil, errors.New("TLS is not initialized")
        }
        if https_client == nil {
            https_client = &http.Client{ Transport: transport }
        }
        client = https_client
        log.Println("Using HTTPS client")
    } else {
        return nil, errors.New(fmt.Sprintf("Unknown schema %s", req.URL.Scheme))
    }
    if err != nil {
        log.Printf("Error preparing a request: %s", err)
    }
    if verbose {
        dump, err := httputil.DumpRequest(req, req.ContentLength < 1024)
        if err == nil {
            log.Println(string(dump))
        } else {
            log.Println(err)
        }
    }
    return client.Do(req)
}

func (handler hopHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
    elapsed := func(start time.Time) {
        e := time.Now().Sub(start) / time.Millisecond
        hopDurations.WithLabelValues("uniform").Observe(float64(e))
    }
    defer elapsed(time.Now())
    if verbose {
        dump, err := httputil.DumpRequest(req, req.ContentLength < 1024)
        if err == nil {
            log.Println(string(dump))
        } else {
            log.Println(err)
        }
    }

    var result [1]string

    hn, _ := os.Hostname()
    path := req.URL.Path
    if len(req.URL.RawPath) > 0 {
        path = req.URL.RawPath
    }
    result[0] = fmt.Sprintf("I am %s, will do %s", hn, path)
    r := result[:]

    split := strings.SplitN(path, "/", 3)
    next_cmd := path
    if len(split) > 1 {
        next_cmd = split[1]
    }
    path = ""
    if len(split) > 2 {
        path = split[2]
    }

    var headers = map[string]string {
        "Content-type": "text/plain",
        "Accept-Encoding": "text/plain",
    }
    var rheaders = map[string]string {
        "Content-type": "text/plain",
    }
    var fheaders []string

    showHeaders := false
    skip := false
    not := false
    code := 0
    size := 0

    q := func (c int) {
        quit <- c
    }

    for strings.HasPrefix(next_cmd, "-") {
        cmd := strings.SplitN(next_cmd, ":", 2)

        split := strings.SplitN(path, "/", 2)
        if len(split) > 0 {
            next_cmd = split[0]
        } else {
            next_cmd = ""
        }
        if len(split) > 1 {
            path = split[1]
        } else {
            path = ""
        }

        if skip {
            r = append(r, fmt.Sprintf("Skipping %s", cmd[0]))
            skip = false
            continue
        }

        switch cmd[0] {
        case "-help":
            for k,v := range help {
                r = append(r, fmt.Sprintf("%-13s - %s", k, v))
            }
            r = append(r, "Examples:")
            r = append(r, "curl -H \"a: b\" hop1/-info")
            r = append(r, "\tthis will call hop1 which will show some details of the request")
            r = append(r, "curl -H \"a: b\" hop1/-fheader:a/hop2")
            r = append(r, "\tthis will call hop1 which will call hop2 with forwarded header A")
            r = append(r, "curl hop1/-rnd:50/hop2/hop3/-on:hop2/-code:500")
            r = append(r, "\tthis will call hop1 which will call hop2 or hop3 (50%). hop2 would call hop3 and return error code 500")
        case "-wait":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            d, err := strconv.Atoi(cmd[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Cannot wait for %s ms", cmd[1]))
                continue
            }
            time.Sleep(time.Duration(d) * time.Millisecond)
            r = append(r, fmt.Sprintf("Waited for %d ms", d))
        case "-info":
            showHeaders = true
            r = append(r, fmt.Sprintf("Got %d bytes from %s", req.ContentLength, req.RemoteAddr))
            dump, err := httputil.DumpRequest(req, req.ContentLength < 1024)
            if err == nil {
                for _, line := range strings.Split(string(dump), "\n") {
                    r = append(r, fmt.Sprintf(".\t%s", line))
                }
            } else {
                r = append(r, fmt.Sprintf("Error: %s", err))
            }
            if req.TLS != nil {
                r = append(r, fmt.Sprintf("TLS version 0x%x, cipher 0x%x, protocol %s, server name %s",
                    req.TLS.Version, req.TLS.CipherSuite, req.TLS.NegotiatedProtocol, req.TLS.ServerName))
            }

        case "-header":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            hv := strings.SplitN(cmd[1], "=", 2)
            if len(hv) != 2 {
                r = append(r, fmt.Sprintf("Missing header value for %s", cmd[0]))
                continue
            }
            value, err := url.PathUnescape(hv[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Bad value for header %s: %s", hv[0], hv[1]))
            } else {
                r = append(r, fmt.Sprintf("Will add header %s: %s", hv[0], value))
                headers[hv[0]] = value
            }
        case "-rheader":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            hv := strings.SplitN(cmd[1], "=", 2)
            if len(hv) != 2 {
                r = append(r, fmt.Sprintf("Missing header value for %s", cmd[0]))
                continue
            }
            value, err := url.PathUnescape(hv[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Bad value for header %s: %s", hv[0], hv[1]))
            } else {
                r = append(r, fmt.Sprintf("Will return header %s: %s", hv[0], value))
                rheaders[hv[0]] = value
            }
        case "-fheader":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            r = append(r, fmt.Sprintf("Will forward header %s: %s", cmd[1], req.Header.Get(cmd[1])))
            headers[cmd[1]] = req.Header.Get(cmd[1])
            fheaders = append(fheaders, cmd[1])
        case "-code":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            c, err := strconv.Atoi(cmd[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Cannot return code %s", cmd[1]))
                continue
            }
            code = c
            r = append(r, fmt.Sprintf("Returning code %d", code))
        case "-rsize":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            b, err := strconv.Atoi(cmd[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Cannot create a byte array of %s bytes", cmd[1]))
                continue
            }
            r = append(r, fmt.Sprintf("Appending %d bytes", b))
            r = append(r, strings.Repeat("X", b))
            r = append(r, "\n")
        case "-env":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            r = append(r, fmt.Sprintf("%s=%s", cmd[1], os.Getenv(cmd[1])))
        case "-size":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            b, err := strconv.Atoi(cmd[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Cannot create a byte array of %s bytes", cmd[1]))
                continue
            }
            size = b
            r = append(r, fmt.Sprintf("Will add %d bytes to the following request", size))
        case "-not":
            not = !not
        case "-on":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            value, err := url.PathUnescape(cmd[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Bad value for host name %s: %s", cmd[1]))
                continue
            }
            skip = !strings.Contains(hn, value)
            if not {
                skip = !skip
                not = false
            }
        case "-if":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            hv := strings.SplitN(cmd[1], "=", 2)
            if len(hv) != 2 {
                r = append(r, fmt.Sprintf("Missing header value for %s", cmd[0]))
                continue
            }
            value, err := url.PathUnescape(hv[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Bad value for header %s: %s", hv[0], hv[1]))
                continue
            }
            skip = !(strings.ToLower(hv[0]) == "host" && strings.Contains(req.Host, value)) && !strings.Contains(req.Header.Get(hv[0]), value)
            if not {
                skip = !skip
                not = false
            }
        case "-rnd":
            if len(cmd) != 2 {
                r = append(r, fmt.Sprintf("Missing parameter for %s", cmd[0]))
                continue
            }
            p, err := strconv.Atoi(cmd[1])
            if err != nil {
                r = append(r, fmt.Sprintf("Cannot make a random of %s", cmd[1]))
                continue
            }
            skip = p <= rand.Intn(100)
            if not {
                skip = !skip
                not = false
            }
        case "-quit":
            r = append(r, "Quitting")
            defer q(1)
        case "-crash":
            defer q(2)
        }
    }

    if skip {
        r = append(r, fmt.Sprintf("Skipping call to %s", next_cmd))
        if code == 0 {
            code = 200
        }
    }
    for !skip && len(next_cmd) > 0 {

        next_host, err := url.PathUnescape(next_cmd)
        if err != nil {
            r = append(r, fmt.Sprintf("Cannot call %s: %s\n", next_cmd, err.Error()))
            break
        }
        if !strings.HasPrefix(next_host, "http://") && !strings.HasPrefix(next_host, "https://") {
            next_host = "http://" + next_host
        }
        u, err := url.Parse(fmt.Sprintf("%s/%s", next_host, path))
        if err != nil {
            r = append(r, fmt.Sprintf("Cannot call %s: %s\n", next_host, err.Error()))
            break
        }
        if u == nil {
            break
        }
        if len(u.Scheme) == 0 {
            u.Scheme = "http"
        }
        var res *http.Response
        req, err = buildRequest(u, &headers, size)
        if err != nil {
            r = append(r, fmt.Sprintf("Couldn't make %s: %s\n", u, err.Error()))
            break
        } else if req == nil {
            r = append(r, fmt.Sprintf("Couldn't make %s by some reason\n", u))
            break
        }
        if proxy_url, _ := proxy(req); proxy_url != nil {
            if verbose {
                log.Printf("Using proxy: %s", proxy_url)
            }
            if !proxy_tunneling {
                req.URL.Host = proxy_url.Host
                r = append(r, fmt.Sprintf("Overriding url: %s", req.URL))
            }
            if showHeaders {
                r = append(r, fmt.Sprintf("Using proxy: %s", proxy_url))
            }
        }
        res, err = callURL(req)
        if err != nil {
            r = append(r, fmt.Sprintf("Couldn't call %s: %s\n", u, err.Error()))
            break
        } else if res == nil {
            r = append(r, fmt.Sprintf("Couldn't call %s by some reason\n", u))
            break
        }
        defer res.Body.Close()

        r = append(r, fmt.Sprintf("Called %s: %s", u, res.Status))
        if showHeaders {
            var dump []byte
            dump, err = httputil.DumpResponse(res, res.ContentLength < 1024)
            if err == nil {
                for _, line := range strings.Split(string(dump), "\n") {
                    r = append(r, fmt.Sprintf(".\t%s", line))
                }
                if res.ContentLength >= 1024 {
                    r = append(r, fmt.Sprintf(".\t<%d bytes>", res.ContentLength))
                }
            } else {
                r = append(r, err.Error())
            }
        }
        if code == 0 {
            code = res.StatusCode
        }

        for _, h := range fheaders {
            v := res.Header.Get(h)
            r = append(r, fmt.Sprintf("Back forwarding header %s: %s", h, v))
            if len(v) > 0 {
                rheaders[h] = v
            }
        }
        if code == 0 && err != nil {
            code = 500
        }
        break
    }

    for h, v := range rheaders {
        w.Header().Set(h, v)
    }
    if code == 0 {
        code = 200
    }
    w.WriteHeader(code)
    for _, line := range r {
        io.WriteString(w, "| ")
        io.WriteString(w, line)
        io.WriteString(w, "\n")
    }
}


var (
    port_http, port_https string
    port_metrics = "8080"
    certificate, key string
    proxy_tunneling = false
    verbose = false
    useTLS = false
    localhost string

    // Create a summary to track fictional interservice HOP latencies for three
    // distinct services with different latency distributions. These services are
    // differentiated via a "service" label.
    hopDurations = prometheus.NewSummaryVec(
        prometheus.SummaryOpts{
            Name:       "hop_durations_seconds",
            Help:       "HOP latency distributions.",
            Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
        },
        []string{"service"},
    )
)

type nullWriter struct {}
func (nw nullWriter) Write(p []byte) (n int, err error) {
    return 0, nil
}

func init() {
    rand.Seed(13)
    port_http = os.Getenv("PORT");
    port_https = os.Getenv("PORT_HTTPS");
    if len(port_http) == 0 {
        port_http = "80"
    }
    if len(port_https) == 0 {
        port_https = "443"
    }
    http_proxy := os.Getenv("http_proxy");
    https_proxy := os.Getenv("https_proxy");
    var cacert string
    flag.BoolVar(&verbose, "verbose", false, "verbose output")
    flag.StringVar(&port_http, "port_http", port_http, "port HTTP")
    flag.StringVar(&port_https, "port_https", port_https, "port HTTPS")
    flag.StringVar(&port_metrics, "port_metrics", port_metrics, "port Prometheus metrics")
    flag.StringVar(&http_proxy, "http_proxy", http_proxy, "HTTP proxy")
    flag.StringVar(&https_proxy, "https_proxy", https_proxy, "HTTPS proxy")
    flag.BoolVar(&proxy_tunneling, "proxy_tunneling", false, "use proxy tunneling (if false just put the proxy to the Host: header)")
    flag.StringVar(&cacert, "cacert", "", "CA certificate")
    flag.StringVar(&certificate, "cert", "", "certificate")
    flag.StringVar(&key, "key", "", "key")
    flag.StringVar(&localhost, "interface", "0.0.0.0", "the interface to listen on")

    flag.Parse()

    if verbose {
        log.SetOutput(os.Stdout)
    } else {
        log.SetOutput(nullWriter{})
    }

    var err error
    if len(https_proxy) != 0 {
        https_proxy_url, err = url.Parse(https_proxy)
    }
    if len(http_proxy) != 0 {
        http_proxy_url, err = url.Parse(http_proxy)
    }
    if err != nil {
        log.Panicf("failed to parse parameters: %s\n", err)
    }

    useTLS = len(certificate) !=0 && len(key) != 0

    if useTLS {
        initTLS(cacert)
    }
    // Register the summary and the histogram with Prometheus's default registry.
    prometheus.MustRegister(hopDurations)
}

func main() {

    s := &http.Server {
        Addr: net.JoinHostPort(localhost, port_http),
        Handler: hopHandler {},
        ReadTimeout: 10 * time.Second,
        WriteTimeout: 10 * time.Second,
        MaxHeaderBytes: 1 << 20,
        ErrorLog: log.New(os.Stdout, "http: ", 0),
    }

    var stls *http.Server

    if useTLS {
        stls = &http.Server {
            Addr: net.JoinHostPort(localhost, port_https),
            Handler: hopHandler {},
            ReadTimeout: 10 * time.Second,
            WriteTimeout: 10 * time.Second,
            MaxHeaderBytes: 1 << 20,
            TLSConfig: tlsServerConfig,
            ErrorLog: log.New(os.Stdout, "https: ", 0),
        }
    }

    metrics := &http.Server {
        Addr: net.JoinHostPort(localhost, port_metrics),
        Handler: promhttp.Handler(),
        ReadTimeout: 10 * time.Second,
        WriteTimeout: 10 * time.Second,
        MaxHeaderBytes: 1 << 20,
        ErrorLog: log.New(os.Stdout, "http: ", 0),
    }

    go func() {
        fmt.Println("Serving HTTP on", localhost, port_http)
        fmt.Println(s.ListenAndServe())
        quit<-3
    }()

    if stls != nil {
        go func() {
            fmt.Println("Serving HTTPS on", localhost, port_https)
            fmt.Println(stls.ListenAndServeTLS(certificate, key))
            quit<-4
        }()
    }

    go func() {
        fmt.Println("Serving /metrics on", localhost, port_metrics)
        // metrics.Handle("/metrics", promhttp.Handler())
        fmt.Println(metrics.ListenAndServe())
        quit<-5
    }()

    switch <-quit {
    case 1:
        fmt.Println("Shutting down")
        err := s.Shutdown(context.Background());
        if err != nil {
            fmt.Printf("Error:", err)
        }
        <-quit
        if stls != nil {
            err = stls.Shutdown(context.Background());
            if err != nil {
                fmt.Printf("Error:", err)
            }
            <-quit
        }
        if err != nil {
            panic("Failed to stop gracefully")
        }
    case 2:
        panic("Rabbits are coming!")
    }
    fmt.Println("Exiting normally")
}

