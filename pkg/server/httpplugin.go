package server

// The [[http_plugins]] webhooks mirror frp's server plugin contract: for every
// operation a plugin declares, the server POSTs a JSON envelope to the plugin's
// endpoint and reads a JSON answer back. A `reject` answer refuses the
// operation with its reason — the session, the registration or the visitor
// connection is turned away the same way a built-in check would turn it away.
// A plugin that cannot be reached rejects too: an operator who wires a gate in
// front of the server does not want it silently bypassed when it is down.
//
// What the envelope carries is deliberately narrower than frp's: no auth
// token and no private proxy secret ever leave the server.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aethertunnel/aethertunnel/pkg/config"
	"github.com/aethertunnel/aethertunnel/pkg/logging"
	"github.com/aethertunnel/aethertunnel/pkg/protocol"
)

// httpPluginAPIVersion is the envelope version, shared with frp's plugin
// contract so an existing frp webhook answers us unchanged.
const httpPluginAPIVersion = "0.1.0"

// httpPluginRequest is the body the server POSTs.
type httpPluginRequest struct {
	Version string `json:"version"`
	Op      string `json:"op"`
	Content any    `json:"content"`
}

// httpPluginResponse is the body the plugin answers with.
type httpPluginResponse struct {
	Reject       bool            `json:"reject"`
	RejectReason string          `json:"reject_reason"`
	Unchange     bool            `json:"unchange"`
	Content      json.RawMessage `json:"content"`
}

// pluginUserInfo says whose session the webhook is being asked about.
type pluginUserInfo struct {
	User          string            `json:"user"`
	RunID         string            `json:"run_id"`
	Metas         map[string]string `json:"metas"`
	ClientVersion string            `json:"client_version,omitempty"`
}

// pluginLoginContent describes the session that asked to log in. The auth
// token is deliberately absent: a webhook has no business holding it.
type pluginLoginContent struct {
	User        pluginUserInfo `json:"user"`
	ClientAddr  string         `json:"client_address"`
	Protocol    int            `json:"protocol"`
	Encrypted   bool           `json:"encrypted"`
	Identity    bool           `json:"identity"`
	PostQuantum bool           `json:"post_quantum"`
	WantsVPN    bool           `json:"wants_vpn"`
}

// pluginProxyContent is the publishable shape of a proxy registration. The
// secret of a private proxy is deliberately absent.
type pluginProxyContent struct {
	User       pluginUserInfo    `json:"user"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	RemotePort int               `json:"remote_port,omitempty"`
	Domains    []string          `json:"domains,omitempty"`
	Subdomain  string            `json:"subdomain,omitempty"`
	Group      string            `json:"group,omitempty"`
	Multipath  int               `json:"multipath,omitempty"`
	HTTPUser   string            `json:"http_user,omitempty"`
	AllowCIDRs []string          `json:"allow_cidrs,omitempty"`
	DenyCIDRs  []string          `json:"deny_cidrs,omitempty"`
	Headers    map[string]string `json:"request_headers,omitempty"`
}

// pluginUserConnContent describes one visitor connection reaching a proxy.
type pluginUserConnContent struct {
	User       pluginUserInfo `json:"user"`
	ProxyName  string         `json:"proxy_name"`
	ProxyType  string         `json:"proxy_type"`
	RemoteAddr string         `json:"remote_addr"`
}

// httpPlugin is one configured webhook endpoint.
type httpPlugin struct {
	name   string
	url    string
	ops    map[string]bool
	client *http.Client
	logger *log.Logger
}

// httpPluginManager dispatches an operation to every plugin that declared it.
type httpPluginManager struct {
	login       []*httpPlugin
	newProxy    []*httpPlugin
	newUserConn []*httpPlugin
	logger      *log.Logger
}

// newHTTPPluginManager builds the webhook set from the configuration. An empty
// configuration returns a manager whose every call is a pass-through.
func newHTTPPluginManager(cfg *config.Config, logger *log.Logger) *httpPluginManager {
	m := &httpPluginManager{logger: logger}
	for i := range cfg.HTTPPlugins {
		options := &cfg.HTTPPlugins[i]
		url := options.Addr + options.Path
		if !strings.Contains(url, "://") {
			url = "http://" + url
		}
		client := &http.Client{}
		if strings.HasPrefix(url, "https://") {
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: !options.TLSVerify},
			}
		}
		timeout := time.Duration(options.TimeoutSecs) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		client.Timeout = timeout

		plugin := &httpPlugin{
			name:   options.Name,
			url:    url,
			ops:    make(map[string]bool, len(options.Ops)),
			client: client,
			logger: logger,
		}
		// ops doubles as the seen-set here: a configuration that lists one op
		// twice would otherwise run the plugin twice per event, and frp's
		// contract is one call per event, which is what counting plugins
		// build on.
		for _, op := range options.Ops {
			if plugin.ops[op] {
				continue
			}
			plugin.ops[op] = true
			switch op {
			case config.HTTPPluginOpLogin:
				m.login = append(m.login, plugin)
			case config.HTTPPluginOpNewProxy:
				m.newProxy = append(m.newProxy, plugin)
			case config.HTTPPluginOpNewUserConn:
				m.newUserConn = append(m.newUserConn, plugin)
			}
		}
	}
	return m
}

// refuseReason is the error an unreachable plugin produces — a gate that is
// down closes the door, it does not hold it open.
func refuseReason(op string) error {
	return fmt.Errorf("send %s request to plugin error", op)
}

// maxPluginResponse bounds the answer one webhook may return.
const maxPluginResponse = 1 << 20

// call POSTs one operation and decodes the answer.
func (p *httpPlugin) call(op string, content any) (*httpPluginResponse, error) {
	body, err := json.Marshal(httpPluginRequest{Version: httpPluginAPIVersion, Op: op, Content: content})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, p.url+"?version="+httpPluginAPIVersion+"&op="+op, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plugin %q answered %s", p.name, response.Status)
	}
	var answer httpPluginResponse
	// The plugin is another process, and a compromised or misbehaving one must
	// not be able to make the server allocate without bound on a login path.
	if err := json.NewDecoder(io.LimitReader(response.Body, maxPluginResponse)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("plugin %q sent an unreadable answer: %w", p.name, err)
	}
	return &answer, nil
}

// userFor builds the user info a webhook sees for one session.
func userFor(session *Session) pluginUserInfo {
	return pluginUserInfo{
		User:          session.ID,
		RunID:         session.ID,
		Metas:         session.Metas,
		ClientVersion: session.ClientVersion,
	}
}

// runLogin runs every login plugin for one auth request. The returned reason,
// empty when every plugin allows, is what the auth response carries.
func (m *httpPluginManager) runLogin(session *Session, req *protocol.AuthRequest) (reason string) {
	if len(m.login) == 0 {
		return ""
	}
	content := pluginLoginContent{
		User:        userFor(session),
		ClientAddr:  session.RemoteAddr,
		Protocol:    req.Protocol,
		Encrypted:   session.Encrypted,
		Identity:    len(req.Identity) > 0,
		PostQuantum: req.KEX != nil,
		WantsVPN:    req.VPN,
	}
	for _, plugin := range m.login {
		answer, err := plugin.call(config.HTTPPluginOpLogin, content)
		if err != nil {
			m.logger.Printf("http plugin %q (login): %v", plugin.name, err)
			return refuseReason(config.HTTPPluginOpLogin).Error()
		}
		if answer.Reject {
			reason := answer.RejectReason
			if reason == "" {
				reason = "rejected by http plugin " + plugin.name
			}
			logging.Warnf(m.logger, "http plugin %q rejected the login from %s: %s", plugin.name, session.RemoteAddr, reason)
			return reason
		}
	}
	return ""
}

// pluginProxyPatch is what a newProxy webhook may rewrite: the publishable
// fields. The secret of a private proxy never round-trips through a webhook.
type pluginProxyPatch struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	RemotePort int      `json:"remote_port"`
	Domains    []string `json:"domains"`
	Subdomain  string   `json:"subdomain"`
	Group      string   `json:"group"`
	Multipath  int      `json:"multipath"`
	HTTPUser   string   `json:"http_user"`
}

// runNewProxy runs every newProxy plugin for one registration. A reject is the
// registration's refusal reason; a plugin that answered unchange=false has its
// publishable fields written back into the spec.
func (m *httpPluginManager) runNewProxy(session *Session, spec *protocol.ProxySpec) error {
	if len(m.newProxy) == 0 {
		return nil
	}
	for _, plugin := range m.newProxy {
		// Rebuilt for every plugin: an earlier rewrite is the spec's current
		// state, and the next plugin must judge and rewrite that rather than
		// the registration as it arrived — and its own rewrite then clobbers
		// the first one wholesale.
		content := pluginProxyContent{
			User:       userFor(session),
			Name:       spec.Name,
			Type:       spec.Type,
			RemotePort: spec.RemotePort,
			Domains:    spec.Domains,
			Subdomain:  spec.Subdomain,
			Group:      spec.Group,
			Multipath:  spec.Multipath,
			HTTPUser:   spec.HTTPUser,
			AllowCIDRs: spec.AllowCIDRs,
			DenyCIDRs:  spec.DenyCIDRs,
			Headers:    spec.RequestHeaders,
		}
		answer, err := plugin.call(config.HTTPPluginOpNewProxy, content)
		if err != nil {
			m.logger.Printf("http plugin %q (newProxy): %v", plugin.name, err)
			return refuseReason(config.HTTPPluginOpNewProxy)
		}
		if answer.Reject {
			reason := answer.RejectReason
			if reason == "" {
				reason = "rejected by http plugin " + plugin.name
			}
			logging.Warnf(m.logger, "http plugin %q rejected the registration of %q: %s", plugin.name, spec.Name, reason)
			return fmt.Errorf("%s", reason)
		}
		if !answer.Unchange && len(answer.Content) > 0 {
			var patch pluginProxyPatch
			if err := json.Unmarshal(answer.Content, &patch); err != nil {
				m.logger.Printf("http plugin %q (newProxy): the rewritten content is unreadable: %v", plugin.name, err)
				return fmt.Errorf("http plugin %q sent an unreadable rewrite", plugin.name)
			}
			spec.Name = patch.Name
			spec.Type = patch.Type
			spec.RemotePort = patch.RemotePort
			spec.Domains = patch.Domains
			spec.Subdomain = patch.Subdomain
			spec.Group = patch.Group
			spec.Multipath = patch.Multipath
			spec.HTTPUser = patch.HTTPUser
		}
	}
	return nil
}

// runNewUserConn runs every newUserConn plugin for one visitor connection. The
// returned reason, empty when every plugin allows, is what the visitor sees.
func (m *httpPluginManager) runNewUserConn(session *Session, group *ProxyGroup, remoteAddr string) (reason string) {
	if len(m.newUserConn) == 0 {
		return ""
	}
	content := pluginUserConnContent{
		User:       userFor(session),
		ProxyName:  group.Name,
		ProxyType:  group.Type,
		RemoteAddr: remoteAddr,
	}
	for _, plugin := range m.newUserConn {
		answer, err := plugin.call(config.HTTPPluginOpNewUserConn, content)
		if err != nil {
			m.logger.Printf("http plugin %q (newUserConn): %v", plugin.name, err)
			return refuseReason(config.HTTPPluginOpNewUserConn).Error()
		}
		if answer.Reject {
			reason := answer.RejectReason
			if reason == "" {
				reason = "rejected by http plugin " + plugin.name
			}
			return reason
		}
	}
	return ""
}
