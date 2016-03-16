package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/engine-api/client/transport/cancellable"

	"golang.org/x/net/context"
)

// serverResponse is a wrapper for http API responses.
type serverResponse struct {
	body       io.ReadCloser
	header     http.Header
	statusCode int
}

// head sends an http request to the docker API using the method HEAD.
func (cli *Client) head(path string, query url.Values, headers map[string][]string) (*serverResponse, error) {
	return cli.sendRequest("HEAD", path, query, nil, headers)
}

// get sends an http request to the docker API using the method GET.
func (cli *Client) get(path string, query url.Values, headers map[string][]string) (*serverResponse, error) {
	return cli.sendRequest("GET", path, query, nil, headers)
}

// post sends an http request to the docker API using the method POST.
func (cli *Client) post(path string, query url.Values, body interface{}, headers map[string][]string) (*serverResponse, error) {
	return cli.sendRequest("POST", path, query, body, headers)
}

// postRaw sends the raw input to the docker API using the method POST.
func (cli *Client) postRaw(path string, query url.Values, body io.Reader, headers map[string][]string) (*serverResponse, error) {
	return cli.sendClientRequest("POST", path, query, body, headers)
}

// put sends an http request to the docker API using the method PUT.
func (cli *Client) put(path string, query url.Values, body interface{}, headers map[string][]string) (*serverResponse, error) {
	return cli.sendRequest("PUT", path, query, body, headers)
}

// putRaw sends the raw input to the docker API using the method PUT.
func (cli *Client) putRaw(path string, query url.Values, body io.Reader, headers map[string][]string) (*serverResponse, error) {
	return cli.sendClientRequest("PUT", path, query, body, headers)
}

// delete sends an http request to the docker API using the method DELETE.
func (cli *Client) delete(path string, query url.Values, headers map[string][]string) (*serverResponse, error) {
	return cli.sendRequest("DELETE", path, query, nil, headers)
}

func (cli *Client) sendRequest(method, path string, query url.Values, body interface{}, headers map[string][]string) (*serverResponse, error) {
	params, err := encodeData(body)
	if err != nil {
		return nil, err
	}

	if body != nil {
		if headers == nil {
			headers = make(map[string][]string)
		}
		headers["Content-Type"] = []string{"application/json"}
	}

	return cli.sendClientRequest(method, path, query, params, headers)
}

func tryProxy(cli *Client) error {
	proxyUrl := cli.transport.Scheme() + "://" + cli.addr + "/v" + cli.version + "/info"
	logrus.Debug("proxy workaround url: " + proxyUrl)
	proxyResp, err := http.Get(proxyUrl)
	if err != nil {
		logrus.Debug("failed to make request " + err.Error())
		return err
	}
	logrus.Debug("made proxy workaround call, got status " + fmt.Sprint(proxyResp.StatusCode))
	if proxyResp.StatusCode == 403 {
		data, _ := ioutil.ReadAll(proxyResp.Body)
		proxyResp.Body.Close()
		matches := regexp.MustCompile(`.*"(http://.*http://.*)".*`).FindSubmatch([]byte(data))
		if len(matches) >= 2 {
			logrus.Debug("making proxy auth " + string(matches[1]))
			resp, _ := http.Get(string(matches[1]))
			resp.Body.Close()
			logrus.Debug("proxy auth got status " + fmt.Sprint(resp.StatusCode))
			return nil
		}
	} else {
		logrus.Debug("non-proxy error")
	}
	return errors.New(fmt.Sprint("can not handle status code", proxyResp.StatusCode))
}

func (cli *Client) sendClientRequest(method, path string, query url.Values, body io.Reader, headers map[string][]string) (*serverResponse, error) {
	retries, _ := strconv.Atoi(os.Getenv("DOCKER_HTTP_RETRY"))
	var (
		resp *serverResponse = nil
		err  error           = errors.New("no requests made")
	)
	for try := 0; try <= retries; try++ {
		resp, err = cli.doSendClientRequest(ctx, method, path, query, body, headers)
		if err == nil {
			break
		}
		logrus.Debug("failed, status " + fmt.Sprint(resp.statusCode))
		if resp.statusCode == 407 || resp.statusCode == 403 || resp.statusCode == -1 {
			tryProxy(cli)
			time.Sleep(1 * time.Second)
		} else {
			logrus.Debug("not-retryable error")
			break
		}
	}
	return resp, err
}

func (cli *Client) doSendClientRequest(method, path string, query url.Values, body io.Reader, headers map[string][]string) (*serverResponse, error) {
	serverResp := &serverResponse{
		body:       nil,
		statusCode: -1,
	}

	expectedPayload := (method == "POST" || method == "PUT")
	if expectedPayload && body == nil {
		body = bytes.NewReader([]byte{})
	}

	req, err := cli.newRequest(method, path, query, body, headers)
	req.URL.Host = cli.addr
	req.URL.Scheme = cli.scheme
	logrus.Debug("calling " + req.URL.String())

	if expectedPayload && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "text/plain")
	}

	userAgent, found := syscall.Getenv("DOCKER_USER_AGENT")
	if found {
		if len(userAgent) == 0 {
			req.Header.Del("User-Agent")
		} else {
			req.Header.Set("User-Agent", userAgent)
		}
	}

	resp, err := cli.httpClient.Do(req)
	if resp != nil {
		serverResp.statusCode = resp.StatusCode
	}

	if err != nil {
		if isTimeout(err) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "dial unix") {
			return serverResp, ErrConnectionFailed
		}

		if cli.scheme == "http" && strings.Contains(err.Error(), "malformed HTTP response") {
			return serverResp, fmt.Errorf("%v.\n* Are you trying to connect to a TLS-enabled daemon without TLS?", err)
		}
		if cli.scheme == "https" && strings.Contains(err.Error(), "remote error: bad certificate") {
			return serverResp, fmt.Errorf("The server probably has client authentication (--tlsverify) enabled. Please check your TLS client certification settings: %v", err)
		}

		return serverResp, fmt.Errorf("An error occurred trying to connect: %v", err)
	}

	if serverResp.statusCode < 200 || serverResp.statusCode >= 400 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return serverResp, err
		}
		if len(body) == 0 {
			return serverResp, fmt.Errorf("Error: request returned %s for API route and version %s, check if the server supports the requested API version", http.StatusText(serverResp.statusCode), req.URL)
		}
		return serverResp, fmt.Errorf("Error response from daemon: %s", bytes.TrimSpace(body))
	}

	serverResp.body = resp.Body
	serverResp.header = resp.Header
	return serverResp, nil
}

func (cli *Client) newRequest(method, path string, query url.Values, body io.Reader, headers map[string][]string) (*http.Request, error) {
	apiPath := cli.getAPIPath(path, query)
	req, err := http.NewRequest(method, apiPath, body)
	if err != nil {
		return nil, err
	}

	// Add CLI Config's HTTP Headers BEFORE we set the Docker headers
	// then the user can't change OUR headers
	for k, v := range cli.customHTTPHeaders {
		req.Header.Set(k, v)
	}

	if headers != nil {
		for k, v := range headers {
			req.Header[k] = v
		}
	}

	return req, nil
}

func encodeData(data interface{}) (*bytes.Buffer, error) {
	params := bytes.NewBuffer(nil)
	if data != nil {
		if err := json.NewEncoder(params).Encode(data); err != nil {
			return nil, err
		}
	}
	return params, nil
}

func ensureReaderClosed(response *serverResponse) {
	if response != nil && response.body != nil {
		response.body.Close()
	}
}

func isTimeout(err error) bool {
	type timeout interface {
		Timeout() bool
	}
	e := err
	switch urlErr := err.(type) {
	case *url.Error:
		e = urlErr.Err
	}
	t, ok := e.(timeout)
	return ok && t.Timeout()
}
