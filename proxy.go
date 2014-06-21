package goproxy

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"os"
	"regexp"
	"sync/atomic"
	"github.com/twinj/uuid"
)

// The basic proxy type. Implements http.Handler.
type ProxyHttpServer struct {
	// session variable must be aligned in i386
	// see http://golang.org/src/pkg/sync/atomic/doc.go#L41
	sess int64
	// setting Verbose to true will log information on each request sent to the proxy
	Verbose       bool
	Logger        *log.Logger
	reqHandlers   []ReqHandler
	respHandlers  []RespHandler
	httpsHandlers []HttpsHandler
	Tr            *http.Transport
	// ConnectDial will be used to create TCP connections for CONNECT requests
	// if nil Tr.Dial will be used
	ConnectDial func(network string, addr string) (net.Conn, error)
}

var hasPort = regexp.MustCompile(`:\d+$`)

func copyHeaders(dst, src http.Header) {
	for k, _ := range dst {
		dst.Del(k)
	}
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isEof(r *bufio.Reader) bool {
	_, err := r.Peek(1)
	if err == io.EOF {
		return true
	}
	return false
}

func (proxy *ProxyHttpServer) filterRequest(r *http.Request, ctx *ProxyCtx) (req *http.Request, resp *http.Response) {
	req = r
	for _, h := range proxy.reqHandlers {
		req, resp = h.Handle(r, ctx)
		// non-nil resp means the handler decided to skip sending the request
		// and return canned response instead.
		if resp != nil {
			break
		}
	}
	return
}
func (proxy *ProxyHttpServer) filterResponse(respOrig *http.Response, ctx *ProxyCtx) (resp *http.Response) {
	resp = respOrig
	for _, h := range proxy.respHandlers {
		ctx.Resp = resp
		resp = h.Handle(resp, ctx)
	}
	return
}

func removeProxyHeaders(ctx *ProxyCtx, r *http.Request) {
	r.RequestURI = "" // this must be reset when serving a request with the client
	ctx.Logf("Sending request %v %v", r.Method, r.URL.String())
	// If no Accept-Encoding header exists, Transport will add the headers it can accept
	// and would wrap the response body with the relevant reader.
	r.Header.Del("Accept-Encoding")
	// curl can add that, see
	// http://homepage.ntlworld.com/jonathan.deboynepollard/FGA/web-proxy-connection-header.html
	r.Header.Del("Proxy-Connection")
	// Connection is single hop Header:
	// http://www.w3.org/Protocols/rfc2616/rfc2616.txt
	// 14.10 Connection
	//   The Connection general-header field allows the sender to specify
	//   options that are desired for that particular connection and MUST NOT
	//   be communicated by proxies over further connections.
	r.Header.Del("Connection")
}

// Standard net/http function. Shouldn't be used directly, http.Serve will use it.
func (proxy *ProxyHttpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	//r.Header["X-Forwarded-For"] = w.RemoteAddr()
	if r.Method == "CONNECT" {
		proxy.handleHttps(w, r)
	} else {
		u := uuid.NewV4()
		ctx := &ProxyCtx{Req: r, Session: atomic.AddInt64(&proxy.sess, 1), proxy: proxy, Uuid: u}

		var err error
		//ctx.Logf("Got request %v %v %v %v", r.URL.Path, r.Host, r.Method, r.URL.String())
		if !r.URL.IsAbs() {
			if r.Host == "" {
				ctx.Warnf("non-proxy request received, without Host header")
				http.Error(w, err.Error(), 500)
				return
			}
			r.URL, err = url.Parse("http://" + r.Host + r.URL.Path)
			if err != nil {
				ctx.Warnf("unparsable path or host received, by non-proxy request: %+#v", r.URL.Path)
				http.Error(w, err.Error(), 500)
				return
			}
		}
		r, resp := proxy.filterRequest(r, ctx)

		if resp == nil {
			removeProxyHeaders(ctx, r)
			resp, err = ctx.RoundTrip(r)
			if err != nil {
				ctx.Error = err
				resp = proxy.filterResponse(nil, ctx)
				if resp == nil {
					ctx.Logf("error read response %v %v:", r.URL.Host, err.Error())
					http.Error(w, err.Error(), 500)
					return
				}
			}
			ctx.Logf("Received response %v", resp.Status)
		}
		origBody := resp.Body
		resp = proxy.filterResponse(resp, ctx)

		ctx.Logf("Copying response to client %v [%d]", resp.Status, resp.StatusCode)
		// http.ResponseWriter will take care of filling the correct response length
		// Setting it now, might impose wrong value, contradicting the actual new
		// body the user returned.
		// We keep the original body to remove the header only if things changed.
		// This will prevent problems with HEAD requests where there's no body, yet,
		// the Content-Length header should be set.
		if origBody != resp.Body {
			resp.Header.Del("Content-Length")
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		// Create a channel to send content to the writer
		c := make(chan []byte)

		// Read body from a goroutine
		go readContent(resp.Body, c)

		spath := getSocketPath("socket_srv")
		ln, err := net.Listen("unix", spath)
		if err != nil {
			log.Print(err)
			return
		}

		log.Printf("Opening socket %s, %+v", spath, ctx)

		defer func(ln net.Listener, spath string){
		    log.Printf("Closing and killing socket %s", spath)
		    ln.Close()
		    os.Remove(spath)
		}(ln, spath)

		// Wait for input to inject to the client
		go waitForMessage(c, ln, ctx)

		for content := range c{
			// write a chunk
			if _, err := w.Write(content); err != nil {
				//panic(err)
				log.Printf("error writing %+v\n", err)
				break
			} else if f, ok := w.(http.Flusher); ok {
				// Response writer with flush support.
				f.Flush()
			}

		}

		log.Printf("Finished to read content, kill me now")

	}
}

func readContent(body io.ReadCloser, c chan<- []byte) {
	// Trying to buffer output for chunked encoding
	buf := make([]byte, 1024)
	for {
		// read a chunk
		n, err := body.Read(buf)
		if err != nil && err != io.EOF {
			close(c)
			panic(err)
		}
		if n == 0 {
			break
		}

		c <- buf[:n]
	}

	if err := body.Close(); err != nil {
		log.Printf("Can't close response body %v", err)
	}

	log.Printf("Closing channel")
	close(c)
}

// New proxy server, logs to StdErr by default
func NewProxyHttpServer() *ProxyHttpServer {
	proxy := ProxyHttpServer{
		Logger:        log.New(os.Stderr, "", log.LstdFlags),
		reqHandlers:   []ReqHandler{},
		respHandlers:  []RespHandler{},
		httpsHandlers: []HttpsHandler{},
		Tr: &http.Transport{TLSClientConfig: tlsClientSkipVerify,
			Proxy: http.ProxyFromEnvironment},
	}
	proxy.ConnectDial = dialerFromEnv(&proxy)
	return &proxy
}

func getLocalIp() (net.IP, error) {
	var ip net.IP;
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ip, err
	}

	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			return ipnet.IP, nil
		}
	}

	return ip, nil
}

func waitForMessage(ch chan<- []byte, ln net.Listener, ctx *ProxyCtx) {

    for {
	/*select {
	case <- quit:
	    log.Printf("quitting loop")
	    return
	default:*/
	    log.Printf("waiting for new connections")
	    // This will block
	    conn, err := ln.Accept()
	    if err != nil {
		    log.Print(err)
		    break
	    }
	    log.Printf("reading message")
	    go readMessage(conn, ch)
	//}
    }

    /*sigc := make(chan os.Signal, 1)
    signal.Notify(sigc, os.Interrupt, os.Kill, syscall.SIGTERM)
    go func(c chan os.Signal, spath string) {
        // Wait for a SIGINT or SIGKILL:
        sig := <-c
        log.Printf("Caught signal %s: shutting down.", sig)
        // Stop listening (and unlink the socket if unix type):
        ln.Close()
        //os.Remove(spath)
        // And we're done:
        os.Exit(0)
    }(sigc, spath)*/

}

func readMessage(c net.Conn, ch chan<- []byte) {
    defer c.Close()

    msg := make([]byte, 1024)

    for {
        n, err := c.Read(msg)

        if err != nil && err != io.EOF {
            log.Printf("ERROR: read\n")
            log.Print(err)
            return
        }

        if n != 0 {
	    ch <- msg[:n]
        }

        if err == io.EOF {
            return
        }
    }
}

func createTempDir(name string) (string, error) {
	tmpdir := filepath.Join(os.TempDir(), name)

        _, err := os.Stat(tmpdir)

	if err != nil {
		if os.IsNotExist(err) {
			// Create temp directory
			err = os.MkdirAll(tmpdir, 0777)
		}

	}

        return tmpdir, err
}

func getSocketPath(name string) (string) {
    tmpdir, err := createTempDir("proxy-sockets")

    // err should be nil if we just created the directory
    if err != nil {
            panic(err)
    }

    spath := filepath.Join(tmpdir, name)
    log.Printf("creating socket: %s", spath)

    return spath
}
