package client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"

	log "github.com/Sirupsen/logrus"
	"github.com/akutz/gofig"
	"github.com/akutz/goof"
	"github.com/akutz/gotil"
	//gjson "github.com/gorilla/rpc/json"
	"golang.org/x/net/context/ctxhttp"

	"github.com/emccode/libstorage/api/types/context"
	httptypes "github.com/emccode/libstorage/api/types/http"
	"github.com/emccode/libstorage/api/utils"
)

// Client is the interface for Golang libStorage clients.
type Client interface {

	// Root returns a list of root resources.
	Root() ([]string, error)

	// Volumes returns a list of all Volumes for all Services.
	Volumes() (httptypes.ServiceVolumeMap, error)
}

type client struct {
	config       gofig.Config
	httpClient   *http.Client
	proto        string
	laddr        string
	tlsConfig    *tls.Config
	logRequests  bool
	logResponses bool
	ctx          context.Context
}

// Dial opens a connection to a remote libStorage serice and returns the client
// that can be used to communicate with said endpoint.
//
// If the config parameter is nil a default instance is created. The
// function dials the libStorage service specified by the configuration
// property libstorage.host.
func Dial(
	ctx context.Context,
	config gofig.Config) (Client, error) {

	c := &client{config: config}
	c.logRequests = c.config.GetBool(
		"libstorage.client.http.logging.logrequest")
	c.logResponses = c.config.GetBool(
		"libstorage.client.http.logging.logresponse")

	logFields := log.Fields{}

	host := config.GetString("libstorage.host")
	if host == "" {
		return nil, goof.New("libstorage.host is required")
	}

	tlsConfig, tlsFields, err :=
		utils.ParseTLSConfig(config.Scope("libstorage.client"))
	if err != nil {
		return nil, err
	}
	c.tlsConfig = tlsConfig
	for k, v := range tlsFields {
		logFields[k] = v
	}

	cProto, cLaddr, err := gotil.ParseAddress(host)
	if err != nil {
		return nil, err
	}
	c.proto = cProto
	c.laddr = cLaddr

	if ctx == nil {
		log.Debug("created empty context for client")
		ctx = context.Background()
	}
	ctx = ctx.WithContextID("host", host)

	c.httpClient = &http.Client{
		Transport: &http.Transport{
			Dial: func(proto, addr string) (conn net.Conn, err error) {
				if tlsConfig == nil {
					return net.Dial(cProto, cLaddr)
				}
				return tls.Dial(cProto, cLaddr, tlsConfig)
			},
		},
	}

	ctx.Log().WithFields(logFields).Info("configured client")

	c.ctx = ctx
	return c, nil
}

func (c *client) Root() ([]string, error) {
	reply := httptypes.RootResponse{}
	if err := c.httpGet("/", &reply); err != nil {
		return nil, err
	}
	return reply, nil
}

func (c *client) Volumes() (httptypes.ServiceVolumeMap, error) {
	reply := httptypes.ServiceVolumeMap{}
	if err := c.httpGet("/volumes", &reply); err != nil {
		return nil, err
	}
	return reply, nil
}

func (c *client) httpDo(method, path string, payload, reply interface{}) error {

	reqBody, err := encPayload(payload)
	if err != nil {
		return err
	}

	host := c.laddr
	if c.proto == "unix" {
		host = "libstorage-server"
	}
	if c.tlsConfig != nil && c.tlsConfig.ServerName != "" {
		host = c.tlsConfig.ServerName
	}

	url := fmt.Sprintf("http://%s%s", host, path)
	c.ctx.Log().WithField("url", url).Debug("built request url")
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return err
	}
	c.logRequest(req)

	res, err := ctxhttp.Do(c.ctx, c.httpClient, req)
	if err != nil {
		return err
	}
	c.logResponse(res)

	if err := decRes(res.Body, reply); err != nil {
		return err
	}

	return nil
}

func (c *client) httpGet(path string, reply interface{}) error {
	return c.httpDo("GET", path, nil, reply)
}

func (c *client) httpPost(
	path string,
	payload interface{}, reply interface{}) error {

	return c.httpDo("POST", path, payload, reply)
}

func (c *client) httpDelete(path string, reply interface{}) error {

	return c.httpDo("DELETE", path, nil, reply)
}

func encPayload(payload interface{}) (io.Reader, error) {
	if payload == nil {
		return nil, nil
	}

	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(buf), nil
}

func decRes(body io.Reader, reply interface{}) error {
	buf, err := ioutil.ReadAll(body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(buf, reply); err != nil {
		return err
	}
	return nil
}

func (c *client) logRequest(req *http.Request) {

	if !c.logRequests {
		return
	}

	w := log.StandardLogger().Writer()

	fmt.Fprintln(w, "")
	fmt.Fprint(w, "    -------------------------- ")
	fmt.Fprint(w, "HTTP REQUEST (CLIENT)")
	fmt.Fprintln(w, " -------------------------")

	buf, err := httputil.DumpRequest(req, true)
	if err != nil {
		return
	}

	gotil.WriteIndented(w, buf)
	fmt.Fprintln(w)
}

func (c *client) logResponse(res *http.Response) {

	if !c.logResponses {
		return
	}

	w := log.StandardLogger().Writer()

	fmt.Fprintln(w)
	fmt.Fprint(w, "    -------------------------- ")
	fmt.Fprint(w, "HTTP RESPONSE (CLIENT)")
	fmt.Fprintln(w, " -------------------------")

	buf, err := httputil.DumpResponse(res, true)
	if err != nil {
		return
	}

	bw := &bytes.Buffer{}
	gotil.WriteIndented(bw, buf)

	scanner := bufio.NewScanner(bw)
	for {
		if !scanner.Scan() {
			break
		}
		fmt.Fprintln(w, scanner.Text())
	}
}

func (c *client) getLocalDevices(prefix string) ([]string, error) {

	path := c.config.GetString("libstorage.client.localdevicesfile")
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	deviceNames := []string{}
	scan := bufio.NewScanner(bytes.NewReader(buf))

	rx := regexp.MustCompile(fmt.Sprintf(`^.+?\s(%s\w+)$`, prefix))
	for scan.Scan() {
		l := scan.Text()
		m := rx.FindStringSubmatch(l)
		if len(m) > 0 {
			deviceNames = append(deviceNames, fmt.Sprintf("/dev/%s", m[1]))
		}
	}

	return deviceNames, nil
}