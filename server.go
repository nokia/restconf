package restconf

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeconf/restconf/device"
	"github.com/freeconf/restconf/secure"
	"github.com/freeconf/restconf/stock"
	"github.com/freeconf/yang/fc"
	"github.com/freeconf/yang/meta"
	"github.com/freeconf/yang/nodeutil"
	"github.com/freeconf/yang/parser"
	"github.com/freeconf/yang/source"
)

type Server struct {
	Web                      *stock.HttpServer
	webApps                  []webApp
	Auth                     secure.Auth
	Ver                      string
	NotifyKeepaliveTimeoutMs int
	main                     device.Device
	devices                  device.Map
	notifiers                *list.List
	ypath                    source.Opener

	// Optional: Anything not handled by RESTCONF protocol can call this handler otherwise
	UnhandledRequestHandler http.HandlerFunc

	// Give app change to read custom header data and stuff into context so info can get
	// to app layer
	Filters []RequestFilter

	// allow rpc to serve under /restconf/data/{module:}/{rpc} which while intuative and
	// original design, it is not in compliance w/RESTCONF spec
	OnlyStrictCompliance bool
}

var ErrBadAddress = errors.New("expected format: http://server/restconf[=device]/operation/module:path")

type RequestFilter func(ctx context.Context, w http.ResponseWriter, r *http.Request) (context.Context, error)

func NewServer(d *device.Local) *Server {
	m := &Server{
		notifiers: list.New(),
		ypath:     d.SchemaSource(),
	}
	m.ServeDevice(d)

	if err := d.Add("fc-restconf", Node(m, d.SchemaSource())); err != nil {
		panic(err)
	}

	// Required by all devices according to RFC
	if err := d.Add("ietf-yang-library", device.LocalDeviceYangLibNode(m.ModuleAddress, d)); err != nil {
		panic(err)
	}
	return m
}

func (srv *Server) Close() error {
	if srv.Web == nil {
		return nil
	}
	err := srv.Web.Server.Close()
	srv.Web = nil
	return err
}

func (srv *Server) ModuleAddress(m *meta.Module) string {
	return fmt.Sprint("schema/", m.Ident(), ".yang")
}

func (srv *Server) DeviceAddress(id string, d device.Device) string {
	return fmt.Sprint("/restconf=", id)
}

func (srv *Server) ServeDevices(m device.Map) error {
	srv.devices = m
	return nil
}

func (srv *Server) ServeDevice(d device.Device) error {
	srv.main = d
	return nil
}

func (srv *Server) determineCompliance(r *http.Request) ComplianceOptions {
	if srv.OnlyStrictCompliance {
		return Strict
	}
	if r.URL.Query().Has(SimplifiedComplianceParam) {
		return Simplified
	}
	contentType := r.Header.Get("Content-Type")
	acceptType := r.Header.Get("Accept")

	if contentType == YangDataJsonMimeType1 || contentType == YangDataJsonMimeType2 || contentType == YangDataXmlMimeType1 || contentType == YangDataXmlMimeType2 || acceptType == YangDataJsonMimeType1 || acceptType == YangDataJsonMimeType2 || acceptType == YangDataXmlMimeType1 || acceptType == YangDataXmlMimeType2 {
		return Strict
	}
	if acceptType == TextStreamMimeType {
		return Strict
	}
	return Simplified
}

func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	compliance := srv.determineCompliance(r)
	fc.Debug.Printf("compliance %s", compliance)
	ctx := context.WithValue(context.Background(), ComplianceContextKey, compliance)
	if fc.DebugLogEnabled() {
		fc.Debug.Printf("%s %s", r.Method, r.URL)
		if r.Body != nil {
			content, rerr := ioutil.ReadAll(r.Body)
			defer r.Body.Close()
			if rerr != nil {
				fc.Err.Printf("error trying to log body content %s", rerr)
			} else {
				if len(content) > 0 {
					fc.Debug.Print(string(content))
					r.Body = ioutil.NopCloser(bytes.NewBuffer(content))
				}
			}
		}
	}
	for _, f := range srv.Filters {
		var err error
		if ctx, err = f(ctx, w, r); err != nil {
			handleErr(compliance, err, r, w)
			return
		}
	}

	h := w.Header()

	// CORS
	h.Set("Access-Control-Allow-Headers", "origin, content-type, accept")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS, DELETE, PATCH")
	h.Set("Access-Control-Allow-Origin", "*")
	if r.URL.Path == "/" {
		switch r.Method {
		case "OPTIONS":
			return
		case "GET":
			if len(srv.webApps) > 0 {
				http.Redirect(w, r, srv.webApps[0].endpoint, http.StatusMovedPermanently)
				return
			}
		}
	}

	op1, deviceId, p := shiftOptionalParamWithinSegment(r.URL, '=', '/')
	device, err := srv.findDevice(deviceId)
	if err != nil {
		handleErr(compliance, err, r, w)
		return
	}
	switch op1 {
	case ".ver":
		w.Write([]byte(srv.Ver))
		return
	case ".well-known":
		srv.serveStaticRoute(w, r)
		return
	case "restconf":
		op2, p := shift(p, '/')
		r.URL = p
		switch op2 {
		case "data":
			srv.serve(compliance, ctx, device, w, r, endpointData)
		case "streams":
			srv.serve(compliance, ctx, device, w, r, endpointStreams)
		case "operations":
			srv.serve(compliance, ctx, device, w, r, endpointOperations)
		case "ui":
			srv.serveStreamSource(compliance, r, w, device.UiSource(), r.URL.Path)
		case "schema":
			// Hack - parse accept header to get proper content type
			accept := r.Header.Get("Accept")
			fc.Debug.Printf("accept %s", accept)
			if strings.Contains(accept, "/json") {
				srv.serveSchema(compliance, ctx, w, r, device.SchemaSource())
			} else {
				srv.serveStreamSource(compliance, r, w, device.SchemaSource(), r.URL.Path)
			}
		default:
			handleErr(compliance, ErrBadAddress, r, w)
		}
		return
	}
	if srv.handleWebApp(w, r, op1, p.Path) {
		return
	}
	if srv.UnhandledRequestHandler != nil {
		srv.UnhandledRequestHandler(w, r)
		return
	}
}

const (
	endpointData = iota
	endpointOperations
	endpointStreams
	endpointSchema
)

func (srv *Server) serveSchema(compliance ComplianceOptions, ctx context.Context, w http.ResponseWriter, r *http.Request, ypath source.Opener) {
	modName, p := shift(r.URL, '/')
	r.URL = p
	m, err := parser.LoadModule(ypath, modName)
	if err != nil {
		handleErr(compliance, err, r, w)
		return
	}
	ylib, err := parser.LoadModule(ypath, "fc-yang")
	if err != nil {
		handleErr(compliance, err, r, w)
		return
	}
	b := nodeutil.Schema(ylib, m)
	hndlr := &browserHandler{browser: b}
	hndlr.ServeHTTP(compliance, ctx, w, r, endpointSchema)
}

func (srv *Server) serve(compliance ComplianceOptions, ctx context.Context, d device.Device, w http.ResponseWriter, r *http.Request, endpointId int) {
	if hndlr, p := srv.shiftBrowserHandler(compliance, r, d, w, r.URL); hndlr != nil {
		r.URL = p
		hndlr.ServeHTTP(compliance, ctx, w, r, endpointId)
	}
}

type webApp struct {
	endpoint string
	homeDir  string
	homePage string
}

func (srv *Server) RegisterWebApp(homeDir string, homePage string, endpoint string) {
	srv.webApps = append(srv.webApps, webApp{
		endpoint: endpoint,
		homeDir:  homeDir,
		homePage: homePage,
	})
}

// Serve web app according to SPA conventions where you serve static assets if
// they exist but if they don't assume, the URL is going to be interpretted
// in browser as route path.
func (srv *Server) handleWebApp(w http.ResponseWriter, r *http.Request, endpoint string, path string) bool {
	for _, wap := range srv.webApps {

		if endpoint == wap.endpoint {

			// if someone type "/app/index.html" then direct them to right spot
			if strings.HasPrefix(path, wap.homePage) {
				// redirect to root path so URL is correct in browser
				http.Redirect(w, r, wap.endpoint, http.StatusMovedPermanently)
				return true
			}

			// redirect to root path so URL is correct in browser
			srv.serveWebApp(w, r, wap, path)
			return true
		}
	}
	return false
}

func (srv *Server) serveWebApp(w http.ResponseWriter, r *http.Request, wap webApp, path string) {
	compliance := Simplified
	var rdr *os.File
	useHomePage := false
	if path == "" {
		useHomePage = true
	} else {
		var ferr error
		rdr, ferr = os.Open(filepath.Join(wap.homeDir, path))
		if ferr != nil {
			if os.IsNotExist(ferr) {
				useHomePage = true
			} else {
				handleErr(compliance, ferr, r, w)
				return
			}
		} else {
			// If you do not find a file, assume it's a path that resolves
			// in client and we send the home page.
			stat, _ := rdr.Stat()
			useHomePage = stat.IsDir()
		}
	}
	var ext string
	if useHomePage {
		var ferr error
		rdr, ferr = os.Open(filepath.Join(wap.homeDir, wap.homePage))
		if ferr != nil {
			if os.IsNotExist(ferr) {
				handleErr(compliance, fc.NotFoundError, r, w)
			} else {
				handleErr(compliance, ferr, r, w)
			}
			return
		}
		ext = ".html"
	} else {
		ext = filepath.Ext(path)
	}
	ctype := mime.TypeByExtension(ext)
	w.Header().Set("Content-Type", ctype)
	if _, err := io.Copy(w, rdr); err != nil {
		handleErr(compliance, err, r, w)
	}
}

func (srv *Server) serveStreamSource(compliance ComplianceOptions, r *http.Request, w http.ResponseWriter, s source.Opener, path string) {
	rdr, err := s(path, "")
	if err != nil {
		handleErr(compliance, err, r, w)
		return
	} else if rdr == nil {
		handleErr(compliance, fc.NotFoundError, r, w)
		return
	}
	ext := filepath.Ext(path)
	ctype := mime.TypeByExtension(ext)
	w.Header().Set("Content-Type", ctype)
	if _, err := io.Copy(w, rdr); err != nil {
		handleErr(compliance, err, r, w)
	}
}

func (srv *Server) findDevice(deviceId string) (device.Device, error) {
	if deviceId == "" {
		return srv.main, nil
	}
	device, err := srv.devices.Device(deviceId)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, fmt.Errorf("%w. device %s", fc.NotFoundError, deviceId)
	}
	return device, nil
}

func (srv *Server) shiftBrowserHandler(compliance ComplianceOptions, r *http.Request, d device.Device, w http.ResponseWriter, orig *url.URL) (*browserHandler, *url.URL) {
	if module, p := shift(orig, ':'); module != "" {
		if browser, err := d.Browser(module); browser != nil {
			return &browserHandler{
				browser: browser,
			}, p
		} else if err != nil {
			handleErr(compliance, err, r, w)
			return nil, orig
		}
	}

	handleErr(compliance, fmt.Errorf("%w. no module found in path", fc.NotFoundError), r, w)
	return nil, orig
}

func (srv *Server) serveStaticRoute(w http.ResponseWriter, r *http.Request) bool {
	_, p := shift(r.URL, '/')
	op, _ := shift(p, '/')
	switch op {
	case "host-meta":
		// RESTCONF Sec. 3.1
		fmt.Fprintf(w, `{ "xrd" : { "link" : { "@rel" : "restconf", "@href" : "/restconf" } } }`)
		return true
	}
	return false
}
