package restconf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"

	"context"

	"github.com/freeconf/yang/fc"
	"github.com/freeconf/yang/meta"
	"github.com/freeconf/yang/node"
	"github.com/freeconf/yang/nodeutil"
)

type browserHandler struct {
	browser *node.Browser
}

var subscribeCount int

const EventTimeFormat = "2006-01-02T15:04:05-07:00"

type ProxyContextKey string

var RemoteIpAddressKey = ProxyContextKey("FC_REMOTE_IP")

const YangDataJsonMimeType = "application/yang-data+json"

const TextStreamMimeType = "text/event-stream"

const PlainJsonMimeType = "application/json"

const SimplifiedComplianceParam = "simplified"

type ComplianceContextKeyType string

var ComplianceContextKey = ComplianceContextKeyType("RESTCONF_COMPLIANCE")

func (hndlr *browserHandler) ServeHTTP(compliance ComplianceOptions, ctx context.Context, w http.ResponseWriter, r *http.Request, endpointId int) {
	var err error
	var payload node.Node
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	if r.RemoteAddr != "" {
		host, _ := ipAddrSplitHostPort(r.RemoteAddr)
		ctx = context.WithValue(ctx, RemoteIpAddressKey, host)
	}
	sel := hndlr.browser.RootWithContext(ctx)
	if sel = sel.FindUrl(r.URL); sel.LastErr == nil {
		hdr := w.Header()
		if sel.IsNil() {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		if handleErr(compliance, err, r, w) {
			return
		}
		isRpcOrAction := r.Method == "POST" && meta.IsAction(sel.Meta())
		if !isRpcOrAction && endpointId == endpointOperations {
			http.Error(w, "{+restconf}/operations is only intended for rpcs", http.StatusBadRequest)
		} else if isRpcOrAction && !compliance.AllowRpcUnderData && endpointId == endpointData {
			isAction := sel.Path.Len() > 2 // otherwise an action and ok
			if !isAction {
				http.Error(w, "rpcs are located at {+restconf}/operations not {+restconf}/data", http.StatusBadRequest)
				return
			}
		}
		switch r.Method {
		case "DELETE":
			// CRUD - Delete
			err = sel.Delete()
		case "GET":
			// compliance note : decided to support notifictions on get by delivering
			// first event, then closing connection.  Spec calls for SSE
			if meta.IsNotification(sel.Meta()) {
				hdr.Set("Content-Type", TextStreamMimeType+"; charset=utf-8")
				hdr.Set("Cache-Control", "no-cache")
				hdr.Set("Connection", "keep-alive")
				hdr.Set("X-Accel-Buffering", "no")
				hdr.Set("Access-Control-Allow-Origin", "*")
				// default is chunked and web browsers don't know to read after each
				// flush
				hdr.Set("Transfer-Encoding", "identity")

				var sub node.NotifyCloser
				flusher, hasFlusher := w.(http.Flusher)
				if !hasFlusher {
					panic("invalid response writer")
				}
				flusher.Flush()

				subscribeCount++
				defer func() {
					subscribeCount--
				}()

				errOnSend := make(chan error, 20)
				sub, err = sel.Notifications(func(n node.Notification) {
					defer func() {
						if r := recover(); r != nil {
							err := fmt.Errorf("recovered while attempting to send notification %s", r)
							errOnSend <- err
						}
					}()

					// write into a buffer so we write data all at once to handle concurrent messages and
					// ensure messages are not corrupted.  We could use a lock, but might cause deadlocks
					var buf bytes.Buffer

					// According to SSE Spec, each event needs following format:
					// data: {payload}\n\n
					fmt.Fprint(&buf, "data: ")
					if !compliance.DisableNotificationWrapper {
						etime := n.EventTime.Format(EventTimeFormat)
						fmt.Fprintf(&buf, `{"ietf-restconf:notification":{"eventTime":"%s","event":`, etime)
					}
					err := n.Event.InsertInto(jsonWtr(compliance, &buf)).LastErr
					if err != nil {
						errOnSend <- err
						return
					}
					if !compliance.DisableNotificationWrapper {
						fmt.Fprint(&buf, "}}")
					}
					fmt.Fprint(&buf, "\n\n")
					_, err = w.Write(buf.Bytes())
					if err != nil {
						errOnSend <- fmt.Errorf("error writing notif. %s", err)
					}
					flusher.Flush()
					fc.Debug.Printf("sent %d bytes in notif", buf.Len())
				})
				if err != nil {
					fc.Err.Print(err)
					return
				}
				defer sub()
				select {
				case <-r.Context().Done():
					// normal client closing subscription
				case err = <-errOnSend:
					fc.Err.Print(err)
				}
				return
			} else {
				// CRUD - Read
				setContentType(compliance, w.Header())
				err = sel.InsertInto(jsonWtr(compliance, w)).LastErr
			}
		case "PATCH":
			// CRUD - Upsert
			var input node.Node
			input, err = requestNode(r)
			if err != nil {
				handleErr(compliance, err, r, w)
				return
			}
			err = sel.UpsertFrom(input).LastErr
		case "PUT":
			// CRUD - Remove and replace
			var input node.Node
			input, err = requestNode(r)
			if err != nil {
				handleErr(compliance, err, r, w)
				return
			}
			err = sel.ReplaceFrom(input)
		case "POST":
			if meta.IsAction(sel.Meta()) {
				// RPC
				a := sel.Meta().(*meta.Rpc)
				var input node.Node
				if a.Input() != nil {
					if input, err = readInput(compliance, r, a); err != nil {
						handleErr(compliance, err, r, w)
						return
					}
				}
				outputSel := sel.Action(input)
				if outputSel.LastErr != nil {
					handleErr(compliance, outputSel.LastErr, r, w)
					return
				}
				if !outputSel.IsNil() && a.Output() != nil {
					setContentType(compliance, w.Header())
					if err = sendActionOutput(compliance, w, outputSel, a); err != nil {
						handleErr(compliance, err, r, w)
						return
					}
				} else {
					err = outputSel.LastErr
				}
			} else {
				// CRUD - Insert
				payload = nodeutil.ReadJSONIO(r.Body)
				err = sel.InsertFrom(payload).LastErr
			}
		case "OPTIONS":
			// NOP
		default:
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
	} else {
		err = sel.LastErr
	}

	if err != nil {
		handleErr(compliance, err, r, w)
	}
}

func setContentType(compliance ComplianceOptions, h http.Header) {
	if compliance.QualifyNamespaceDisabled {
		h.Set("Content-Type", mime.TypeByExtension(".json"))
	} else {
		h.Set("Content-Type", YangDataJsonMimeType)
	}
}

func sendActionOutput(compliance ComplianceOptions, out io.Writer, output node.Selection, a *meta.Rpc) error {
	if !compliance.DisableActionWrapper {
		// IETF formated output
		// https://datatracker.ietf.org/doc/html/rfc8040#section-3.6.2
		mod := meta.OriginalModule(a).Ident()
		if _, err := fmt.Fprintf(out, `{"%s:output":`, mod); err != nil {
			return err
		}
	}
	err := output.InsertInto(jsonWtr(compliance, out)).LastErr

	if !compliance.DisableActionWrapper {
		if _, err := fmt.Fprintf(out, "}"); err != nil {
			return err
		}
	}
	return err
}

func jsonWtr(compliance ComplianceOptions, out io.Writer) node.Node {
	wtr := &nodeutil.JSONWtr{
		Out:              out,
		QualifyNamespace: !compliance.QualifyNamespaceDisabled,
	}
	return wtr.Node()
}

func readInput(compliance ComplianceOptions, r *http.Request, a *meta.Rpc) (node.Node, error) {
	// not part of spec, custom feature to allow for form uploads
	if isMultiPartForm(r.Header) {
		return formNode(r)
	} else if compliance.DisableActionWrapper {
		return nodeutil.ReadJSONIO(r.Body), nil
	}

	// IETF formated input
	// https://datatracker.ietf.org/doc/html/rfc8040#section-3.6.1
	var vals map[string]interface{}
	d := json.NewDecoder(r.Body)
	err := d.Decode(&vals)
	if err != nil {
		return nil, err
	}
	key := meta.OriginalModule(a).Ident() + ":input"
	payload, found := vals[key].(map[string]interface{})
	if !found {
		return nil, fmt.Errorf("'%s' missing in input wrapper", key)
	}
	return nodeutil.ReadJSONValues(payload), nil
}

func requestNode(r *http.Request) (node.Node, error) {
	// not part of spec, custom feature to allow for form uploads
	if isMultiPartForm(r.Header) {
		return formNode(r)
	}

	return nodeutil.ReadJSONIO(r.Body), nil
}
