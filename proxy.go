package goproxy

import (
	"bufio"
	"github.com/twinj/uuid"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"
)

// The basic proxy type. Implements http.Handler.
type ProxyHttpServer struct {
	// session variable must be aligned in i386
	// see http://golang.org/src/pkg/sync/atomic/doc.go#L41
	sess int64
	// setting Verbose to true will log information on each request sent to the proxy
	Verbose         bool
	Logger          *log.Logger
	NonproxyHandler http.Handler
	reqHandlers     []ReqHandler
	respHandlers    []RespHandler
	httpsHandlers   []HttpsHandler
	Tr              *http.Transport
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
	r.Header.Del("Proxy-Authenticate")
	r.Header.Del("Proxy-Authorization")
	// Connection, Authenticate and Authorization are single hop Header:
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
			proxy.NonproxyHandler.ServeHTTP(w, r)
			return
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

		//ctx.Logf("UUID: %s", ctx.Uuid.String())
		WriteBody(ctx, resp.Body, w, ctx.Uuid.String())
		//ctx.Logf("Finished to read content, kill me now")

		ctx.Logf("Finished to read/write Body, Closing")
		if err := resp.Body.Close(); err != nil {
			ctx.Logf("Can't close response body %v", err)
		}

	}
}

func WriteBody(ctx *ProxyCtx, reader io.ReadCloser, writer io.Writer, socketName string) {
	// Create a channel to send content to the client
	c := make(chan []byte)
	quit := make(chan bool)

	// Read body from a goroutine
	go readContent(ctx, reader, c, quit)

	spath := getSocketPath(ctx, socketName)

	ln, err := net.Listen("unix", spath)
	if err != nil {
		ctx.Logf("err creating socket %s", err)
		return
	}

	defer func(ln net.Listener, spath string) {
		ln.Close()
		os.Remove(spath)
	}(ln, spath)

	// Wait for input to inject to the client
	go waitForMessage(ctx, c, ln, quit)

	for content := range c {
		// write a chunk
		if _, err := writer.Write(content); err != nil {
			//panic(err)
			ctx.Logf("error writing %+v\n", err)
			break
		} else if flusher, ok := writer.(http.Flusher); ok {
			// Response writer with flush support.
			flusher.Flush()
		}

	}
}

func readContent(ctx *ProxyCtx, body io.ReadCloser, c chan<- []byte, quit chan bool) {
	// Trying to buffer output for chunked encoding
	buf := make([]byte, 1024)

	for {
		// read a chunk
		n, err := body.Read(buf)
		if n > 0 {
			res := make([]byte, n)
			copy(res, buf[:n])
			select {
			case c <- res:

			case <-time.After(1 * time.Second): //adjust the time as needed
				//handle the timeout (return an error or download the file yourself?)
				goto sleep
			}
			//c <- buf[:n]
		}
		if err != nil {
			//close(c)
			//c <- nil
			break
			//panic(err)
		}
	}

sleep:

	if ctx.Delay != 0 {
		ctx.Logf("Sleeping for %d seconds before closing", ctx.Delay)
		time.Sleep(time.Second * time.Duration(ctx.Delay))
	}

	close(c)
	select {
	case quit <- true:
	case <-time.After(1 * time.Second): //adjust the time as needed
		//handle the timeout (return an error or download the file yourself?)
		ctx.Logf("waiting for quit timeout")
	default:
	}
	close(quit)
}

// New proxy server, logs to StdErr by default
func NewProxyHttpServer() *ProxyHttpServer {
	proxy := ProxyHttpServer{
		Logger:        log.New(os.Stderr, "", log.LstdFlags),
		reqHandlers:   []ReqHandler{},
		respHandlers:  []RespHandler{},
		httpsHandlers: []HttpsHandler{},
		NonproxyHandler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Error(w, "This is a proxy server. Does not respond to non-proxy requests.", 500)
		}),
		Tr: &http.Transport{TLSClientConfig: tlsClientSkipVerify,
			Proxy: http.ProxyFromEnvironment},
	}
	proxy.ConnectDial = dialerFromEnv(&proxy)
	return &proxy
}

func getLocalIp() (net.IP, error) {
	var ip net.IP
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

func waitForMessage(ctx *ProxyCtx, ch chan<- []byte, ln net.Listener, quit chan bool) {

	for {
		/*select {
		case <- quit:
		    log.Printf("quitting loop")
		    return
		default:*/
		// This will block
		conn, err := ln.Accept()
		if err != nil {
			ctx.Logf("err waiting for new connection %s", err)
			break
		}
		go readMessage(ctx, conn, ch, quit)
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

func readMessage(ctx *ProxyCtx, c net.Conn, ch chan<- []byte, quit chan bool) {
	defer c.Close()

	msg := make([]byte, 1024)

	for {
		n, err := c.Read(msg)

		if err != nil && err != io.EOF {
			ctx.Logf("error reading input message %s", err)
			return
		}

		if n != 0 {
			select {
			case <-quit:
				ctx.Logf("ending readMessage (quit)")
				return
			default:
			}
			res := make([]byte, n)
			copy(res, msg[:n])
			select {
			case ch <- res:
			case <-time.After(5 * time.Second): //adjust the time as needed
				//handle the timeout (return an error or download the file yourself?)
				ctx.Logf("write timeout")
				return
			default:
			}
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

func getSocketPath(ctx *ProxyCtx, name string) string {
	tmpdir, err := createTempDir("proxy-sockets")

	// err should be nil if we just created the directory
	if err != nil {
		panic(err)
	}

	spath := filepath.Join(tmpdir, name)

	return spath
}
