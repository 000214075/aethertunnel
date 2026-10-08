#!/usr/bin/env python3
"""Drives the dashboard in a real browser and checks what it does.

The panel is a single HTML file with inline CSS and JS that talks to the dashboard
API. Nothing else in this repository runs it: the Go tests check the API, and the
end-to-end suites check the tunnels. This script is the missing half — it starts
Chrome with the remote debugging port open, speaks the DevTools protocol over a
WebSocket, and asserts on the page the way a person using it would: the token
prompt, the tiles and tables against the API's own answer, a language switch, the
two banners, the disconnect button, and the drawer on a narrow viewport.

It needs python3 (already required by the other scripts) and a Chrome-like binary.
With no browser installed it prints SKIP and exits 0, so callers can run it
unconditionally.

usage: panel-checks.py --url http://127.0.0.1:7500 --token <dashboard token>
                       [--audit <audit jsonl>] [--chrome <binary>] [--keep-profile]
                       [--bare-url <url> --bare-token <token>]

The tokens may also come from AETHERTUNNEL_PANEL_TOKEN and
AETHERTUNNEL_PANEL_BARE_TOKEN, which keeps them out of the process list.
"""
import argparse
import base64
import json
import os
import re
import shutil
import socket
import struct
import subprocess
import sys
import time
import traceback
import urllib.request

PASSES = 0
FAILURES = 0


def ok(label):
    global PASSES
    PASSES += 1
    print("PASS  %s" % label)


def bad(label, detail):
    global FAILURES
    FAILURES += 1
    print("FAIL  %s -> %s" % (label, detail))


def check(label, got, want):
    if got == want:
        ok(label)
    else:
        bad(label, "want %r got %r" % (want, got))


def check_true(label, got):
    if got:
        ok(label)
    else:
        bad(label, "want true got %r" % (got,))


# --- the smallest DevTools-protocol client this needs -------------------------

class WebSocket:
    """A client-side WebSocket: masked text frames, no extensions, no fragments."""

    def __init__(self, url, timeout=20):
        assert url.startswith("ws://"), url
        rest = url[len("ws://"):]
        hostport, _, path = rest.partition("/")
        host, _, port = hostport.partition(":")
        self.sock = socket.create_connection((host, int(port) or 80), timeout=timeout)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((
            "GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n" % (path, hostport, key)
        ).encode())
        header = b""
        while b"\r\n\r\n" not in header:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("the DevTools socket closed during the handshake")
            header += chunk
        first_line = header.split(b"\r\n", 1)[0]
        if b"101" not in first_line:
            raise RuntimeError("the WebSocket handshake was refused: %r" % first_line)
        self._buffer = header.split(b"\r\n\r\n", 1)[1]

    def _read(self, count):
        while len(self._buffer) < count:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError("the DevTools socket closed")
            self._buffer += chunk
        out, self._buffer = self._buffer[:count], self._buffer[count:]
        return out

    def send(self, text):
        payload = text.encode()
        mask = os.urandom(4)
        header = bytearray([0x81])
        length = len(payload)
        if length < 126:
            header.append(0x80 | length)
        elif length < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", length)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", length)
        header += mask
        self.sock.sendall(bytes(header) + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))

    def recv(self):
        while True:
            first, second = self._read(2)
            opcode = first & 0x0F
            length = second & 0x7F
            if length == 126:
                length = struct.unpack(">H", self._read(2))[0]
            elif length == 127:
                length = struct.unpack(">Q", self._read(8))[0]
            data = self._read(length)
            if opcode == 0x1:
                return data.decode()
            if opcode == 0x8:
                raise RuntimeError("the DevTools socket was closed by the browser")

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


class Browser:
    """Chrome with its debugging port open, and one page attached to it."""

    def __init__(self, binary, profile, width=1400, height=900, log=None):
        self.port = _free_port()
        self.log = open(log, "wb") if log else subprocess.DEVNULL
        self.process = subprocess.Popen([
            binary, "--headless=new", "--disable-gpu", "--no-first-run",
            "--no-default-browser-check", "--disable-extensions", "--disable-dev-shm-usage",
            "--user-data-dir=" + profile,
            "--remote-debugging-port=%d" % self.port,
            "--window-size=%d,%d" % (width, height),
            "about:blank",
        ], stdout=self.log, stderr=self.log)
        self.ws = None
        self.session = None
        self._id = 0
        self.on_event = None
        try:
            self._wait_for_port()
        except Exception:
            # A half-built browser never reaches the caller, so nothing else would
            # close the process or free the profile.
            self.close()
            raise

    def _wait_for_port(self):
        deadline = time.time() + 30
        while time.time() < deadline:
            try:
                with urllib.request.urlopen("http://127.0.0.1:%d/json/version" % self.port, timeout=2) as resp:
                    version = json.loads(resp.read())
                self.ws = WebSocket(version["webSocketDebuggerUrl"])
                break
            except Exception:
                if self.process.poll() is not None:
                    raise RuntimeError("the browser exited before opening its debugging port")
                time.sleep(0.2)
        else:
            raise RuntimeError("the browser never opened its debugging port")
        target = self.call("Target.createTarget", {"url": "about:blank"})
        attached = self.call("Target.attachToTarget", {"targetId": target["targetId"], "flatten": True})
        self.session = attached["sessionId"]
        self.call("Page.enable", session=True)
        self.call("Runtime.enable", session=True)
        # The accessibility tree is read later; enabling it here keeps that read from
        # being the first thing the domain hears about.
        self.call("Accessibility.enable", session=True)

    def _send(self, method, params=None, session=True):
        """Sends without waiting: used from an event handler, where waiting would
        deadlock against the command that is already in flight."""
        self._id += 1
        message = {"id": self._id, "method": method}
        if params:
            message["params"] = params
        if session:
            message["sessionId"] = self.session
        self.ws.send(json.dumps(message))
        return self._id

    def call(self, method, params=None, session=False, timeout=30):
        wanted = self._send(method, params, session)
        deadline = time.time() + timeout
        while time.time() < deadline:
            event = json.loads(self.ws.recv())
            if event.get("id") == wanted:
                if "error" in event:
                    raise RuntimeError("%s: %s" % (method, event["error"]))
                return event.get("result", {})
            if event.get("method") and self.on_event:
                self.on_event(event)
        raise RuntimeError("%s did not answer within %ss" % (method, timeout))

    def evaluate(self, expression, timeout=30):
        result = self.call("Runtime.evaluate", {
            "expression": expression, "returnByValue": True, "awaitPromise": True,
        }, session=True, timeout=timeout)
        if "exceptionDetails" in result:
            raise RuntimeError("evaluating %r failed: %s" % (
                expression[:80], json.dumps(result["exceptionDetails"])[:300]))
        return result.get("result", {}).get("value")

    def navigate(self, url, timeout=30):
        self.call("Page.navigate", {"url": url}, session=True)
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.evaluate("document.readyState") == "complete":
                return
            time.sleep(0.1)
        raise RuntimeError("the page never finished loading: %s" % url)

    def wait_for(self, expression, timeout=15, label=None):
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            last = self.evaluate(expression)
            if last:
                return last
            time.sleep(0.1)
        raise RuntimeError("timed out waiting for %s (last value %r)" % (label or expression, last))

    def text(self, element_id):
        return self.evaluate("document.getElementById(%r).textContent" % element_id)

    def close(self):
        try:
            self.call("Browser.close", timeout=5)
        except Exception:
            pass
        try:
            self.process.wait(timeout=10)
        except Exception:
            self.process.kill()
        if self.ws:
            self.ws.close()


def _free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def chrome_binary():
    for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
        found = shutil.which(name)
        if found:
            return found
    return None


# --- the checks ----------------------------------------------------------------

def api(url, token, path):
    request = urllib.request.Request(url.rstrip("/") + path)
    request.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(request, timeout=10) as resp:
        return json.loads(resp.read())


def fetch_text(url, token, path):
    """The page's own bytes, so the checks can look at the markup as well as at what
    the browser made of it."""
    request = urllib.request.Request(url.rstrip("/") + path)
    request.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(request, timeout=10) as resp:
        return resp.read().decode("utf-8")


def dictionary_audit(html):
    """Reads the two dictionaries and the keys the markup annotates.

    The page compares its dictionaries at start-up and logs a mismatch to the console,
    where nobody sees it: a key missing from one language leaves that string in the
    other language, and a key nothing mentions is dead weight in a file that is
    compiled into the server. Returns a report, so the caller decides what to assert.
    """
    outer = re.search(r"var I18N = \{(.*?)\n  \};", html, re.S)
    if not outer:
        return None
    blocks = {}
    # The two dictionaries close differently: English is followed by the Chinese one,
    # Chinese by the end of the object.
    for lang, pattern in (("en", r"\n    en: \{(.*?)\n    \},"),
                          ("zh", r"\n    zh: \{(.*?)\n    \}")):
        found = re.search(pattern, outer.group(1), re.S)
        if not found:
            return None
        blocks[lang] = re.findall(r"'([^']+)':", found.group(1))
    en, zh = blocks["en"], blocks["zh"]
    annotated = set(re.findall(r'data-i18n(?:-label|-placeholder)?="([^"]+)"', html))
    # The dictionaries themselves are removed before looking for uses: a key's own
    # definition contains the key, and counting that as a reference would make the
    # "no dead keys" check pass on every file.
    rest = html[:outer.start()] + html[outer.end():]
    return {
        "en_keys": en,
        "zh_keys": zh,
        "duplicates": sorted({k for k in en if en.count(k) > 1} | {k for k in zh if zh.count(k) > 1}),
        "missing_in_zh": sorted(set(en) - set(zh)),
        "missing_in_en": sorted(set(zh) - set(en)),
        "annotated": sorted(annotated),
        "undefined": sorted(k for k in annotated if k not in en or k not in zh),
        "unreferenced": sorted(k for k in en if ("'%s'" % k) not in rest and ('"%s"' % k) not in rest),
    }


def wait_until(browser, expression, timeout=8):
    """wait_for, for a condition whose absence is a check result.

    wait_for raises on timeout, which is right for a step the rest of the checks depend
    on; a check that asserts something the page should have done wants a FAIL with a
    label, not a stack trace."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if browser.evaluate(expression):
            return True
        time.sleep(0.2)
    return False


def pause_poll(browser):
    """Makes the page look hidden so its own 2 s poll stops.

    The poll's success path calls hideError(), which hides the banner and clears the
    remembered failure; a poll landing between a failed request and the assertion
    would clear the very banner a check is looking at. Failing /api/status instead
    would just raise the status banner and replace the one under test, so the poll
    has to be stopped."""
    browser.evaluate("Object.defineProperty(document, 'hidden', "
                     "{ get: function () { return true; }, configurable: true })")


def resume_poll(browser):
    browser.evaluate("delete document.hidden")


def check_directory_off(browser, note):
    """Asserts the directory view of a deployment whose [dht] section is disabled."""
    browser.evaluate("document.getElementById('navDirectory').click()")
    browser.wait_for("document.getElementById('view-directory').hidden === false",
                     label="the directory view")
    browser.wait_for("document.getElementById('dhtState').textContent !== '\\u2014'",
                     label="the directory state row")
    check("a deployment with the directory off says so (%s)" % note,
          browser.text("dhtState"), "no")
    check("and has no node to show (%s)" % note, browser.text("dhtAddr"), "—")
    check_true("it explains that nothing is announced (%s)" % note,
               "directory is off" in browser.text("dhtAnnouncedEmpty"))
    check_true("and draws no announced table (%s)" % note,
               browser.evaluate("document.getElementById('dhtAnnouncedTable').hidden"))


def check_ledger_off(browser, note):
    """Asserts the ledger view of a deployment whose [ledger] section is disabled.

    "Off" is a state of its own: the page has to say so instead of drawing zeroes that
    would read as "nothing was used".
    """
    browser.evaluate("document.getElementById('navLedger').click()")
    browser.wait_for("document.getElementById('view-ledger').hidden === false",
                     label="the ledger view")
    browser.wait_for("document.getElementById('ledgerState').textContent !== '\\u2014'",
                     label="the ledger state row")
    check("a deployment with the ledger off says so (%s)" % note,
          browser.text("ledgerState"), "no")
    check("and has no verification key to show (%s)" % note,
          browser.text("ledgerPublicKey"), "—")
    check_true("it explains that nothing is recorded (%s)" % note,
               "ledger is off" in browser.text("ledgerEntriesEmpty"))
    check_true("and draws no entries table (%s)" % note,
               browser.evaluate("document.getElementById('ledgerEntriesTable').hidden"))
    check_true("and no totals table (%s)" % note,
               browser.evaluate("document.getElementById('ledgerTotalsTable').hidden"))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True, help="the dashboard's base URL")
    parser.add_argument("--token", default=os.environ.get("AETHERTUNNEL_PANEL_TOKEN", ""),
                        help="[dashboard] token (or set AETHERTUNNEL_PANEL_TOKEN, which keeps it out of ps)")
    parser.add_argument("--audit", default="", help="audit log to confirm an action against")
    parser.add_argument("--chrome", default="", help="browser binary to use")
    parser.add_argument("--keep-profile", action="store_true")
    parser.add_argument("--bare-url", default="",
                        help="a second dashboard with [ledger] and [dht] off, for the off states")
    parser.add_argument("--bare-token", default=os.environ.get("AETHERTUNNEL_PANEL_BARE_TOKEN", ""),
                        help="its [dashboard] token (or AETHERTUNNEL_PANEL_BARE_TOKEN)")
    args = parser.parse_args()
    if not args.token:
        parser.error("--token or AETHERTUNNEL_PANEL_TOKEN is required")
    if args.bare_url and not args.bare_token:
        parser.error("--bare-token or AETHERTUNNEL_PANEL_BARE_TOKEN is required with --bare-url")

    binary = args.chrome or chrome_binary()
    if not binary:
        print("SKIP  the dashboard checks: no chrome, chromium or google-chrome on this machine")
        return 0
    if not os.access(binary, os.X_OK):
        print("the browser given on the command line is not executable: %s" % binary, file=sys.stderr)
        return 2

    profile = "/tmp/aethertunnel-panel-%d" % os.getpid()
    log = "/tmp/aethertunnel-panel-chrome-%d.log" % os.getpid()
    browser = None
    try:
        browser = Browser(binary, profile, log=log)
        run_checks(browser, args)
    except Exception as exc:
        # A helper that raises would otherwise abort every remaining check with a
        # traceback and no FAIL line, and the caller would count the run as short
        # rather than failed.
        traceback.print_exc()
        bad("the dashboard checker ran to completion", "%s: %s" % (type(exc).__name__, exc))
    finally:
        if browser is not None:
            try:
                browser.close()
            except Exception:
                pass
        if not args.keep_profile:
            shutil.rmtree(profile, ignore_errors=True)
            try:
                os.remove(log)
            except OSError:
                pass

    print()
    print("dashboard checks passed: %d" % PASSES)
    print("dashboard checks failed: %d" % FAILURES)
    return 1 if FAILURES else 0


def run_checks(browser, args):
    url, token = args.url, args.token
    # The checks compare the page against the API's own answer, so an unreachable
    # dashboard is reported as such rather than as a traceback.
    try:
        status = api(url, token, "/api/status")
        clients = api(url, token, "/api/clients")["clients"]
        proxies = api(url, token, "/api/proxies")["proxies"]
        config = api(url, token, "/api/config")
    except Exception as exc:
        bad("the dashboard API answers", "%s did not: %s" % (url, exc))
        return

    # --- the token prompt ---------------------------------------------------
    browser.navigate(url + "/")
    check_true("the dashboard loads", browser.evaluate("document.title") != "")
    # The overlay is un-hidden by the page's own script, which can land a moment
    # after the load event on a loaded machine: reading it once raced that script
    # (the check below, milliseconds later, saw it up). The wait is bounded and
    # still before any token exists, which is what the check is about.
    overlay_deadline = time.time() + 10
    while time.time() < overlay_deadline:
        if browser.evaluate("!document.getElementById('authOverlay').hidden"):
            break
        time.sleep(0.2)
    check_true("the token prompt is up before a token is known",
               browser.evaluate("!document.getElementById('authOverlay').hidden"))
    check_true("the prompt is visible, not just present",
               browser.evaluate("getComputedStyle(document.getElementById('authOverlay')).display !== 'none'"))
    note = browser.text("authStateNote")
    check_true("the prompt says a token is needed", "token" in note.lower())

    # The observable effect is the rejection message; the earlier version asserted
    # the `true` this expression returns, which passes however the submit is handled.
    browser.evaluate("""
        (function () {
          document.getElementById('authInput').value = 'not-the-token';
          document.getElementById('authForm').dispatchEvent(
            new Event('submit', { cancelable: true, bubbles: true }));
          return true;
        })()
    """)
    check_true("the prompt accepts a submission and reports the rejection",
               wait_until(browser, "document.getElementById('authError').hidden === false", timeout=15))
    check_true("a rejected token keeps the prompt up",
               browser.evaluate("!document.getElementById('authOverlay').hidden"))
    check_true("a rejected token is reported",
               "401" in browser.text("authError"))

    browser.evaluate("""
        (function () {
          document.getElementById('authInput').value = %s;
          document.getElementById('authForm').dispatchEvent(
            new Event('submit', { cancelable: true, bubbles: true }));
          return true;
        })()
    """ % json.dumps(token))
    browser.wait_for("document.getElementById('authOverlay').hidden === true",
                     label="the prompt to close after the right token")
    ok("the right token closes the prompt")
    browser.wait_for("document.getElementById('metricConnMax').textContent !== '\\u2014'",
                     label="the first poll to fill the tiles")

    # --- the tiles against the API -----------------------------------------
    # Re-read the API here instead of trusting the reading taken before Chrome even
    # started: the connection counters are monotonic, so one control connection that
    # opened in that window would fail a page that is right.
    try:
        status = api(url, token, "/api/status")
        clients = api(url, token, "/api/clients")["clients"]
        proxies = api(url, token, "/api/proxies")["proxies"]
    except Exception as exc:
        bad("the dashboard API answers while the tiles are checked", "%s: %s" % (url, exc))
        return
    check("the version tile matches /api/status", browser.text("statusVersion"), status["version"])
    check("the protocol tile matches /api/status",
          browser.text("statusProtocol"), "v%d" % status["protocol"])
    check("the encryption tile matches /api/status", browser.text("statusEncryption"), status["encryption"])
    check("the active-connection tile matches",
          browser.text("metricConnActive"), "{:,}".format(status["connections"]["active"]))
    check("the total-connection tile matches",
          browser.text("metricConnTotal"), "{:,}".format(status["connections"]["total"]))
    check("the authenticated tile matches",
          browser.text("metricConnAuth"), "{:,}".format(status["connections"]["authenticated"]))
    check("the maximum tile matches",
          browser.text("metricConnMax"), "{:,}".format(status["connections"]["max"]))
    check("the registered-proxy tile matches",
          browser.text("metricProxyRegistered"), "{:,}".format(status["proxies"]["registered"]))
    check("the client count matches",
          browser.text("metricClientCount"), "{:,}".format(len(clients)))
    # The row has four documented states; a healthy log is the one an ordinary run
    # sees, and a degraded one is left to the API's own words.
    audit = status["audit"]
    if not audit.get("enabled"):
        check("the audit row reports that the log is off", browser.text("statusAudit"), "no")
    elif audit.get("writable") and not audit.get("records_lost") and not audit.get("recovered"):
        check("the audit row reports a healthy log", browser.text("statusAudit"), "recording")
    else:
        check_true("the audit row reports the state the API reports",
                   browser.text("statusAudit") not in ("no", "recording"))

    # --- the tables against the API ----------------------------------------
    table = browser.evaluate("""
        (function () {
          var rows = document.querySelectorAll('#clientsBody tr');
          return Array.prototype.map.call(rows, function (row) {
            return Array.prototype.map.call(row.children, function (cell) {
              return cell.textContent.trim();
            });
          });
        })()
    """)
    check("the clients table lists every connected client", len(table), len(clients))
    if table and clients:
        check_true("a client row carries the id /api/clients reports",
                   table[0][0] == clients[0]["id"])
        check_true("a client row carries the remote address /api/clients reports",
                   table[0][1] == clients[0]["remote_addr"])

    proxy_rows = browser.evaluate("""
        (function () {
          var rows = document.querySelectorAll('#proxiesBody tr');
          return Array.prototype.map.call(rows, function (row) {
            return Array.prototype.map.call(row.children, function (cell) {
              return cell.textContent.trim();
            });
          });
        })()
    """)
    names = browser.evaluate("""
        (function () {
          var rows = document.querySelectorAll('#proxiesBody tr');
          return Array.prototype.map.call(rows, function (row) { return row.children[0].textContent.trim(); });
        })()
    """)
    check("the proxies table lists every registered proxy",
          sorted(names), sorted([p["name"] for p in proxies]))
    pooled = [p for p in proxies if p.get("member_count", 0) > 1]
    if pooled:
        index = [p["name"] for p in proxies].index(pooled[0]["name"])
        check_true("a pooled proxy shows its member count (%s)" % pooled[0]["name"],
                   "%d" % pooled[0]["member_count"] in proxy_rows[index][5])
    else:
        print("SKIP  the pooled-proxy row: this deployment has no name served by two clients")

    # --- the configuration view -------------------------------------------
    browser.evaluate("document.getElementById('navConfig').click()")
    browser.wait_for("document.getElementById('view-config').hidden === false",
                     label="the configuration view")
    browser.wait_for("document.getElementById('cfgBindAddr').textContent !== '\\u2014'",
                     label="the configuration fields")
    check("the bind address comes from /api/config",
          browser.text("cfgBindAddr"), config["server"]["bind_addr"])
    check("the bind port comes from /api/config",
          browser.text("cfgBindPort"), str(config["server"]["bind_port"]))
    check("the connection limit comes from /api/config",
          browser.text("cfgMaxConn"), str(config["server"]["max_connections"]))
    check("the proxy policy count comes from /api/config",
          browser.text("cfgProxies"), "{:,}".format(config["proxies_configured"]))
    ok("the configuration view renders the server's own settings")

    # --- the layer-3 tunnel -------------------------------------------------
    # The tunnel lives inside the server process and opens only where a tun device can be
    # created, so this state is normally reachable only on a machine that can run the
    # tunnel suite (root, /dev/net/tun). The panel draws it from the same section of
    # /api/config that GET /api/vpn returns.
    before = api(url, token, "/api/vpn")
    vpn = before
    vpn_fields = ["vpnDevice", "vpnServerAddress", "vpnSubnet", "vpnMTU", "vpnPool",
                  "vpnPeers", "vpnFromDevice", "vpnToDevice", "vpnDropped"]
    browser.evaluate("document.getElementById('navConfig').click()")
    browser.wait_for("document.getElementById('view-config').hidden === false",
                     label="the configuration view")
    check("the tunnel panel reports whether the tunnel is on", browser.text("vpnState"),
          "yes" if vpn.get("enabled") else "no")
    if not vpn.get("enabled"):
        check("and leaves every tunnel row empty",
              [browser.text(f) for f in vpn_fields], ["—"] * len(vpn_fields))
        print("SKIP  the tunnel figures: this deployment has [vpn] disabled")
    else:
        browser.wait_for("document.getElementById('vpnDevice').textContent !== '\\u2014'",
                         label="the tunnel panel")
        check("the interface comes from the tunnel", browser.text("vpnDevice"), vpn["device"])
        check("the server's tunnel address comes from the tunnel",
              browser.text("vpnServerAddress"), vpn["server_address"])
        check("the subnet comes from the tunnel", browser.text("vpnSubnet"), vpn["subnet"])
        check("the MTU comes from the tunnel", browser.text("vpnMTU"), str(vpn["mtu"]))
        check("the address pool shows what is handed out of how many",
              browser.text("vpnPool"), "%d / %d" % (vpn["addresses_used"], vpn["pool_size"]))
        check("the peer count comes from the tunnel",
              browser.text("vpnPeers"), str(vpn["peers"]))
        # The counters move while traffic flows, so the page's numbers are required to
        # lie in the window the API reported around the moment it drew them, not to equal
        # one reading taken at a different time.
        packets_before = vpn.get("packets") or {}
        shown_from = browser.text("vpnFromDevice")
        shown_to = browser.text("vpnToDevice")
        after = api(url, token, "/api/vpn")
        packets_after = after.get("packets") or {}
        check_true("the packets read from the interface are the tunnel's own count",
                   shown_from.isdigit()
                   and int(packets_before.get("from_device") or 0) <= int(shown_from)
                   <= int(packets_after.get("from_device") or 0))
        check_true("and the packets delivered to it are too",
                   shown_to.isdigit()
                   and int(packets_before.get("to_device") or 0) <= int(shown_to)
                   <= int(packets_after.get("to_device") or 0))
        check("the lost packets are the unroutable and dropped ones together",
              browser.text("vpnDropped"),
              str((packets_before.get("unroutable") or 0) + (packets_before.get("dropped") or 0)))
        # The two endpoints carry the same section; a panel that reads one while the other
        # moves on is how a view ends up describing a tunnel that is not there. The fields
        # that change with traffic are left out of the comparison.
        stable = ("enabled", "device", "server_address", "subnet", "mask", "mtu", "pool_size")
        check("/api/vpn and /api/config describe the same tunnel",
              {k: after.get(k) for k in stable},
              {k: api(url, token, "/api/config")["vpn"].get(k) for k in stable})
        moved = (packets_after.get("from_device") or 0) + (packets_after.get("to_device") or 0)
        if moved > 0:
            ok("the tunnel really moved packets (%d from the interface, %d to it, %d peer(s))"
               % (packets_after.get("from_device") or 0, packets_after.get("to_device") or 0,
                  after.get("peers")))
        else:
            print("SKIP  the tunnel's packet counters: nothing has crossed the device yet")

    # The configuration view draws yes/no through t(), so its values have to follow a
    # language switch the way its labels do.
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'zh';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    check("the configuration values follow the language switch",
          [browser.text("cfgEncEnabled"), browser.text("cfgAuthRequired"), browser.text("vpnState")],
          ["是" if config["encryption"]["enabled"] else "否",
           "是" if config["dashboard"]["auth_required"] else "否",
           "是" if vpn.get("enabled") else "否"])
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'en';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)

    # --- the same markup, read as text: the dictionaries ---------------------
    # The page's own start-up check only logs to the console, so a key that exists in
    # one language and not the other is invisible in a normal session.
    audit = dictionary_audit(fetch_text(url, token, "/"))
    if audit is None:
        bad("the page's dictionaries can be read from its markup", "the I18N object was not found")
    else:
        # The counts alone are not enough: one key missing from English and a
        # different one missing from Chinese leaves the two counts equal, so the
        # sets have to be asserted, not their sizes.
        check("no key is missing from Chinese", audit["missing_in_zh"], [])
        check("no key is missing from English", audit["missing_in_en"], [])
        # An element whose key is in neither dictionary is missing from both, so the
        # two sets above stay empty and the runtime scan below only inspects
        # [data-i18n]: the label and placeholder attributes would show a raw key with
        # nothing here to say so.
        check("every annotated key exists in both dictionaries", audit["undefined"], [])
        check("no key is defined twice", audit["duplicates"], [])
        check("no key is dead weight", audit["unreferenced"], [])
        ok("%d keys in both languages, every annotated element covered"
           % len(audit["en_keys"]))
        # What the browser actually drew: a key with no entry falls back to the key
        # itself, which is what a reader would see in place of a sentence.
        raw = browser.evaluate("""
            (function () {
              var out = [];
              document.querySelectorAll('[data-i18n]').forEach(function (el) {
                var key = el.getAttribute('data-i18n');
                if (el.textContent.trim() === key) out.push(key);
              });
              return out;
            })()
        """)
        check("no element shows its key instead of a sentence", raw, [])

    # --- the ledger view ----------------------------------------------------
    # The ledger is the record of usage that leaves the server: the panel is the only
    # place its numbers are visible without reading the file by hand.
    ledger = api(url, token, "/api/ledger")
    browser.evaluate("document.getElementById('navLedger').click()")
    browser.wait_for("document.getElementById('view-ledger').hidden === false",
                     label="the ledger view")
    if not ledger.get("enabled"):
        check_ledger_off(browser, "this deployment")
        print("SKIP  the ledger figures: this deployment has [ledger] disabled")
    else:
        browser.wait_for("document.getElementById('ledgerCount').textContent !== '\\u2014'",
                         label="the ledger to load")
        check("the ledger view reports it is on", browser.text("ledgerState"), "yes")
        check("the ledger file comes from /api/ledger", browser.text("ledgerPath"), ledger["path"])
        check("the entry count comes from /api/ledger",
              browser.text("ledgerCount"), "{:,}".format(ledger["count"]))
        check("the verification key comes from /api/ledger",
              browser.text("ledgerPublicKey"), ledger["public_key"])
        check("and is available in full, not only abbreviated",
              browser.evaluate("document.getElementById('ledgerPublicKey').title"),
              ledger["public_key"])
        head = ledger["head"] or ""
        want_head = "—" if not head else (head if len(head) <= 20
                                          else head[:12] + "\u2026" + head[-6:])
        check("the chain head is shown in the same abbreviated form as a hash",
              browser.text("ledgerHead"), want_head)
        check("with the whole hash behind it",
              browser.evaluate("document.getElementById('ledgerHead').title"), head)
        check_true("the live, not-yet-billed total is drawn",
                   browser.text("ledgerUnbilled") != "—")

        totals = ledger.get("totals") or {}
        rows = browser.evaluate("""
            (function () {
              return Array.prototype.map.call(document.querySelectorAll('#ledgerTotalsBody tr'),
                function (row) {
                  return Array.prototype.map.call(row.children, function (c) { return c.textContent.trim(); });
                });
            })()
        """)
        check("the totals table has one row per client in the ledger",
              len(rows), len(totals))
        check("and lists the same clients",
              sorted(r[0] for r in rows), sorted(totals.keys()))
        # One client's usage, expressed the way the page expresses it. A byte count
        # below a kilobyte is written as a whole number of bytes, which needs no second
        # implementation of the page's formatter here; above that the check would only
        # be re-asserting the formatter, so it is skipped instead.
        probe = None
        for row in rows:
            sum_ = totals.get(row[0]) or {}
            if not all(isinstance(sum_.get(k), int) and sum_[k] < 1024
                       for k in ("bytes_in", "bytes_out")):
                continue
            probe = (row, sum_)
            break
        if probe:
            row, sum_ = probe
            check("a client's ledger entry count matches /api/ledger",
                  row[1], "{:,}".format(sum_["entries"]))
            check("a client's billed bytes-in match /api/ledger",
                  row[2], "%d B" % sum_["bytes_in"])
            check("a client's billed bytes-out match /api/ledger",
                  row[3], "%d B" % sum_["bytes_out"])
        else:
            print("SKIP  the ledger byte cells: no client in the ledger is under a kilobyte")

        entries = ledger.get("entries") or []
        erows = browser.evaluate("""
            (function () {
              return Array.prototype.map.call(document.querySelectorAll('#ledgerEntriesBody tr'),
                function (row) {
                  return Array.prototype.map.call(row.children, function (c) { return c.textContent.trim(); });
                });
            })()
        """)
        check("the entries table shows as many rows as /api/ledger returned",
              len(erows), len(entries))
        # A table that drew no rows is a failure to report, not a stack trace: the row
        # is read defensively below.
        first = browser.evaluate("""
            (function () {
              var row = document.querySelector('#ledgerEntriesBody tr');
              if (!row) return null;
              return {
                cells: Array.prototype.map.call(row.children, function (c) { return c.textContent.trim(); }),
                hash: row.children.length > 6 ? row.children[6].title : null,
                timeLabel: row.children.length > 1 ? row.children[1].getAttribute('data-label') : null
              };
            })()
        """)
        if not entries:
            print("SKIP  the ledger rows: the chain is empty on this deployment")
        else:
            check_true("the entries table has a row for the first entry", first is not None)
        if entries and first:
            check("the first row is the first entry /api/ledger returned",
                  first["cells"][0], "{:,}".format(entries[0]["index"]))
            check("its client is the one the ledger recorded",
                  first["cells"][2], entries[0]["client_id"])
            check("its proxy is the one the ledger recorded",
                  first["cells"][3], entries[0]["proxy"])
            check("the entry carries the hash the ledger signed", first["hash"], entries[0]["hash"])
            check_true("the time is drawn as a time",
                       first["cells"][1] not in ("", "—", entries[0]["time"]))
            check_true("the row count is announced",
                       str(len(entries)) in browser.text("ledgerShown").replace(",", ""))
            ok("the ledger view renders the chain the API publishes")

        # The two tables are drawn from the payload, so a language switch has to reach
        # them — a table of English column labels under a Chinese heading is the bug.
        browser.evaluate("""
            (function () {
              var select = document.getElementById('langSelect');
              select.value = 'zh';
              select.dispatchEvent(new Event('change', { bubbles: true }));
              return true;
            })()
        """)
        check("the ledger headings follow the language switch",
              browser.evaluate("document.querySelector('[data-i18n=\"ledger.entriesTitle\"]').textContent"),
              "最近的记录")
        check("the ledger state row follows it too", browser.text("ledgerState"), "是")
        if entries:
            check("and the rows are redrawn with their new column names",
                  browser.evaluate("""
                      (function () {
                        var row = document.querySelector('#ledgerEntriesBody tr');
                        var cell = row && row.children[1];
                        return cell ? cell.getAttribute('data-label') : null;
                      })()
                  """), "时间")
        browser.evaluate("""
            (function () {
              var select = document.getElementById('langSelect');
              select.value = 'en';
              select.dispatchEvent(new Event('change', { bubbles: true }));
              return true;
            })()
        """)
        ok("the ledger view is redrawn when the language changes")

        # A ledger the browser cannot read has to say so the way the other views do: the
        # banner names the endpoint, and the tables stop pretending they have data.
        # The page's own poll calls hideError() on success, which would clear the
        # banner this block asserts on; stop it for the duration.
        pause_poll(browser)
        fail_ledger = {"on": True}

        def intercept_ledger(event):
            if event.get("method") != "Fetch.requestPaused":
                return
            params = event["params"]
            if fail_ledger["on"] and "/api/ledger" in params.get("request", {}).get("url", ""):
                browser._send("Fetch.failRequest", {"requestId": params["requestId"], "errorReason": "Failed"})
            else:
                browser._send("Fetch.continueRequest", {"requestId": params["requestId"]})

        browser.on_event = intercept_ledger
        browser.call("Fetch.enable", {"patterns": [{"urlPattern": "*/api/ledger*"}]}, session=True)
        browser.evaluate("document.getElementById('ledgerRefresh').click()")
        raised = wait_until(browser, "document.getElementById('errorBanner').hidden === false")
        check_true("a failed ledger request raises the banner", raised)
        if raised:
            check_true("and it names the endpoint",
                       "Failed to load /api/ledger" in browser.text("errorBannerText"))
        check_true("and the ledger tables stop showing entries",
                   browser.evaluate("document.getElementById('ledgerEntriesTable').hidden"))
        fail_ledger["on"] = False
        browser.on_event = None
        browser.call("Fetch.disable", session=True)
        browser.evaluate("document.getElementById('ledgerRefresh').click()")
        check_true("the ledger view recovers when the request works again",
                   wait_until(browser, "document.getElementById('errorBanner').hidden === true"
                                       " && (document.getElementById('ledgerEntriesTable').hidden === false"
                                       " || document.getElementById('ledgerEntriesEmpty').hidden === false)"))
        resume_poll(browser)

    # --- the directory view -------------------------------------------------
    # The DHT is how a client finds a server by name, and this view is the only place an
    # operator can see whether this node announces anything and who it knows.
    directory = api(url, token, "/api/dht")
    browser.evaluate("document.getElementById('navDirectory').click()")
    browser.wait_for("document.getElementById('view-directory').hidden === false",
                     label="the directory view")
    if not directory.get("enabled"):
        check_directory_off(browser, "this deployment")
        print("SKIP  the directory figures: this deployment has [dht] disabled")
    else:
        browser.wait_for("document.getElementById('dhtAddr').textContent !== '\\u2014'",
                         label="the directory to load")
        check("the directory view reports it is on", browser.text("dhtState"), "yes")
        check("the bound address comes from /api/dht", browser.text("dhtAddr"), directory["addr"])
        check("the namespace comes from /api/dht",
              browser.text("dhtNamespace"), directory["namespace"])
        check("the peer count comes from /api/dht",
              browser.text("dhtContacts"), "{:,}".format(directory["contacts"]))
        check("the advertised host comes from /api/dht",
              browser.text("dhtAdvertiseAs"), directory["advertise_as"])
        node_id = directory["node_id"] or ""
        check("the node id is abbreviated, like a hash", browser.text("dhtNodeId"),
              "—" if not node_id else (node_id if len(node_id) <= 20
                                       else node_id[:12] + "\u2026" + node_id[-6:]))
        check("with the whole identifier behind it",
              browser.evaluate("document.getElementById('dhtNodeId').title"), node_id)
        # An unsigned node is a fact about it rather than a missing value.
        check("the announcement key is the one the node signs with",
              browser.text("dhtKey"), directory["signing_key"] or "unsigned")

        names = directory.get("announced") or []
        rows = browser.evaluate("""
            (function () {
              return Array.prototype.map.call(document.querySelectorAll('#dhtAnnouncedBody tr'),
                function (row) { return row.children[0].textContent.trim(); });
            })()
        """)
        check("the announced table lists exactly what /api/dht announces",
              sorted(rows), sorted(names))
        if names:
            check_true("the announced count is on screen",
                       str(len(names)) in browser.text("dhtAnnouncedCount"))
        else:
            check_true("an empty announcement list says so",
                       not browser.evaluate("document.getElementById('dhtAnnouncedEmpty').hidden"))

        browser.evaluate("""
            (function () {
              var select = document.getElementById('langSelect');
              select.value = 'zh';
              select.dispatchEvent(new Event('change', { bubbles: true }));
              return true;
            })()
        """)
        check("the directory state row follows the language switch", browser.text("dhtState"), "是")
        check("the announced heading follows it too",
              browser.evaluate("document.querySelector('[data-i18n=\"dht.announcedTitle\"]').textContent"),
              "已通告的名字")
        if names:
            check("and the rows are redrawn with their new column name",
                  browser.evaluate("""
                      (function () {
                        var row = document.querySelector('#dhtAnnouncedBody tr');
                        var cell = row && row.children[0];
                        return cell ? cell.getAttribute('data-label') : null;
                      })()
                  """), "名字")
        browser.evaluate("""
            (function () {
              var select = document.getElementById('langSelect');
              select.value = 'en';
              select.dispatchEvent(new Event('change', { bubbles: true }));
              return true;
            })()
        """)
        ok("the directory view is redrawn when the language changes")

        # The same failure path as the ledger: the banner names the endpoint, and the
        # table stops pretending it has data.
        pause_poll(browser)
        fail_dht = {"on": True}

        def intercept_dht(event):
            if event.get("method") != "Fetch.requestPaused":
                return
            params = event["params"]
            if fail_dht["on"] and "/api/dht" in params.get("request", {}).get("url", ""):
                browser._send("Fetch.failRequest", {"requestId": params["requestId"], "errorReason": "Failed"})
            else:
                browser._send("Fetch.continueRequest", {"requestId": params["requestId"]})

        browser.on_event = intercept_dht
        browser.call("Fetch.enable", {"patterns": [{"urlPattern": "*/api/dht*"}]}, session=True)
        browser.evaluate("document.getElementById('directoryRefresh').click()")
        raised = wait_until(browser, "document.getElementById('errorBanner').hidden === false")
        check_true("a failed directory request raises the banner", raised)
        if raised:
            check_true("and it names the endpoint",
                       "Failed to load /api/dht" in browser.text("errorBannerText"))
        fail_dht["on"] = False
        browser.on_event = None
        browser.call("Fetch.disable", session=True)
        browser.evaluate("document.getElementById('directoryRefresh').click()")
        check_true("the directory view recovers when the request works again",
                   wait_until(browser, "document.getElementById('errorBanner').hidden === true"
                                       " && document.getElementById('dhtAddr').textContent !== '\\u2014'"))
        resume_poll(browser)

    # --- a language switch, with a banner on screen ------------------------
    # The configuration request is failed at the browser, which is what a user sees
    # when the server is briefly unreachable from the page.
    # While the Fetch domain is enabled every matching request is paused until it is
    # answered, so the ones that are not being failed have to be continued explicitly:
    # a paused request is what a hung request looks like to the page.
    # The configuration banner is asserted across a language switch, and a poll that
    # lands in between would clear it; stop the poll until this block is done.
    pause_poll(browser)
    fail_config = {"on": True}

    def intercept(event):
        if event.get("method") != "Fetch.requestPaused":
            return
        params = event["params"]
        if fail_config["on"] and "/api/config" in params.get("request", {}).get("url", ""):
            browser._send("Fetch.failRequest", {"requestId": params["requestId"], "errorReason": "Failed"})
        else:
            browser._send("Fetch.continueRequest", {"requestId": params["requestId"]})

    browser.on_event = intercept
    browser.call("Fetch.enable", {"patterns": [{"urlPattern": "*/api/config*"}]}, session=True)
    browser.evaluate("document.getElementById('navConfig').click(); document.getElementById('configRefresh').click()")
    browser.wait_for("document.getElementById('errorBanner').hidden === false", label="the error banner")
    english = browser.text("errorBannerText")
    check_true("a failed request raises the banner", "Failed to load /api/config" in english)
    ok("the banner names the endpoint and the reason")

    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'zh';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    chinese = browser.text("errorBannerText")
    check_true("the banner follows the language switch", "加载 /api/config 失败" in chinese)
    check_true("the banner keeps the server's reason", "Failed" in chinese or "failed" in chinese.lower())
    check("the connection state follows the switch", browser.text("connStateText"), "已连接")
    check_true("the client count follows the switch", "已连接" in browser.text("clientsCount"))
    check_true("the nav follows the switch", "概览" in browser.evaluate(
        "document.querySelector('[data-i18n=\"nav.overview\"]').textContent"))

    # A failing poll draws the other banner, which has to follow too.
    fail_config["on"] = False
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'en';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    browser.evaluate("document.getElementById('configRefresh').click()")
    browser.wait_for("document.getElementById('errorBanner').hidden === true",
                     label="the banner to clear once the request works")
    ok("the banner clears when the request works again")
    resume_poll(browser)

    fail_status = {"on": True}

    def intercept_status(event):
        if event.get("method") != "Fetch.requestPaused":
            return
        params = event["params"]
        if fail_status["on"] and "/api/status" in params.get("request", {}).get("url", ""):
            browser._send("Fetch.failRequest", {"requestId": params["requestId"], "errorReason": "Failed"})
        else:
            browser._send("Fetch.continueRequest", {"requestId": params["requestId"]})

    browser.on_event = intercept_status
    browser.call("Fetch.enable", {"patterns": [{"urlPattern": "*/api/status*"}]}, session=True)
    # The panel polls on its own timer, so nothing has to be poked: the next poll fails.
    browser.wait_for("document.getElementById('connBanner').hidden === false",
                     timeout=12, label="the disconnected banner")
    ok("a failing /api/status raises the disconnected banner")
    english_banner = browser.text("connBannerText")
    check_true("the disconnected banner is drawn in English", english_banner.startswith("Disconnected"))
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'zh';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    check_true("the disconnected banner follows the switch",
               browser.text("connBannerText").startswith("连接断开"))
    check("the badge follows the switch", browser.text("connStateText"), "连接断开 — 正在重试")
    fail_status["on"] = False
    browser.on_event = None
    browser.call("Fetch.disable", session=True)
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'en';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    browser.evaluate("document.getElementById('errorBannerDismiss').click()")
    check_true("the error banner is dismissible",
               browser.evaluate("document.getElementById('errorBanner').hidden"))
    browser.wait_for("document.getElementById('connBanner').hidden === true",
                     label="the disconnected banner to clear once the polls work again")
    ok("the disconnected banner clears when the polls work again")

    # --- the phone layout: stacked cards, nothing off the side --------------
    browser.call("Emulation.setDeviceMetricsOverride",
                 {"width": 380, "height": 800, "deviceScaleFactor": 1, "mobile": True}, session=True)
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'en';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    check_true("the panel sees a narrow viewport",
               browser.evaluate("window.matchMedia('(max-width: 860px)').matches"))
    check_true("the columns are out of the way below 860 px",
               browser.evaluate("getComputedStyle(document.getElementById('menuToggle')).display !== 'none'"))
    check_true("the sidebar is an off-canvas drawer",
               browser.evaluate("getComputedStyle(document.getElementById('sidebar')).position === 'fixed'"))
    browser.evaluate("document.getElementById('menuToggle').click()")
    check_true("the hamburger opens the drawer",
               browser.evaluate("document.getElementById('sidebar').classList.contains('is-open')"))
    check_true("the scrim is visible behind it",
               browser.evaluate("!document.getElementById('scrim').hidden"))
    check_true("the drawer button says it is expanded",
               browser.evaluate("document.getElementById('menuToggle').getAttribute('aria-expanded')") == "true")
    browser.evaluate("document.getElementById('navProxies').click()")
    check_true("picking a section closes the drawer",
               browser.evaluate("!document.getElementById('sidebar').classList.contains('is-open')"))
    check_true("picking a section shows that view",
               browser.evaluate("!document.getElementById('view-proxies').hidden"))
    check_true("the other views stay hidden",
               browser.evaluate("document.getElementById('view-overview').hidden"))
    check_true("the drawer button says it is closed again",
               browser.evaluate("document.getElementById('menuToggle').getAttribute('aria-expanded')") == "false")
    check_true("the section that is showing is the current one in the markup",
               browser.evaluate("document.getElementById('navProxies').getAttribute('aria-current')") == "page"
               and browser.evaluate("document.getElementById('navOverview').getAttribute('aria-current')") is None)

    # Each row has to be a card whose cells carry their column name, which is what the
    # 700 px rule promises. The label comes from the header of the same column. A
    # deployment with nothing registered has no row to lay out, which is a SKIP rather
    # than a failure: the assertions below are about a row.
    proxy_rows_present = browser.evaluate("document.querySelectorAll('#proxiesBody tr').length") > 0
    cards = browser.evaluate("""
        (function () {
          var headers = Array.prototype.map.call(document.querySelectorAll('#proxiesTable thead th'),
            function (th) { return th.textContent.trim(); });
          var cells = Array.prototype.slice.call(document.querySelectorAll('#proxiesBody tr:first-child td'));
          return cells.map(function (td) {
            return { display: getComputedStyle(td).display,
                     label: (getComputedStyle(td, '::before').content || '').replace(/^"|"$/g, ''),
                     headers: headers };
          });
        })()
    """)
    if proxy_rows_present:
        check_true("every cell of a row is a card block",
                   all(c["display"] in ("flex", "block") for c in cards) and len(cards) > 0)
        check_true("every card carries its column name",
                   all(c["label"] and c["label"] in c["headers"] for c in cards))
    else:
        print("SKIP  the card layout: this deployment has no registered proxy to lay out")

    # The hard case for the promise above: a proxy name no phone can fit on one line.
    # It has to wrap inside the card instead of widening it, and nothing may stick out
    # of the viewport — checked over the whole page, not just that cell.
    narrow = browser.evaluate("""
        (function () {
          var vw = window.innerWidth, bad = [];
          document.querySelectorAll('*').forEach(function (el) {
            var r = el.getBoundingClientRect();
            if (r.width > 0 && r.right > vw + 1) bad.push(el.tagName + '.' + (el.className || ''));
          });
          var cell = null;
          document.querySelectorAll('#proxiesBody td').forEach(function (td) {
            if (td.textContent.indexOf('panel-layout-long-name') === 0) cell = td;
          });
          return { viewport: vw, scrollWidth: document.documentElement.scrollWidth,
                   stickingOut: bad.slice(0, 4), count: bad.length,
                   cell: cell ? { client: cell.clientWidth, scroll: cell.scrollWidth,
                                  height: Math.round(cell.getBoundingClientRect().height) } : null };
        })()
    """)
    check_true("the page does not scroll sideways at 380 px",
               narrow["scrollWidth"] <= narrow["viewport"] and narrow["count"] == 0)
    if narrow["cell"]:
        check_true("a name too long for the screen wraps inside its card",
                   narrow["cell"]["scroll"] <= narrow["cell"]["client"] + 1 and narrow["cell"]["height"] > 20)
    else:
        print("SKIP  the long-name card: this deployment has no proxy named panel-layout-long-name*")

    # The ledger view has its own long values — a 64-character public key, a hash — and
    # two more tables, so the same scan runs over it while the viewport is still narrow.
    # The public key is the interesting one: it has no space to break at, so the row it
    # sits in is where a value that cannot wrap would widen the page.
    if ledger.get("enabled") and ledger.get("count", 0) > 0:
        browser.evaluate("document.getElementById('navLedger').click()")
        ledger_narrow = browser.evaluate("""
            (function () {
              var vw = window.innerWidth, bad = [];
              document.querySelectorAll('#view-ledger *').forEach(function (el) {
                var r = el.getBoundingClientRect();
                if (r.width > 0 && r.right > vw + 1) bad.push(el.tagName + '.' + (el.className || ''));
              });
              var headers = Array.prototype.map.call(
                document.querySelectorAll('#ledgerEntriesTable thead th'),
                function (th) { return th.textContent.trim(); });
              var cells = Array.prototype.slice.call(
                document.querySelectorAll('#ledgerEntriesBody tr:first-child td'));
              var key = document.getElementById('ledgerPublicKey');
              return {
                viewport: vw, scrollWidth: document.documentElement.scrollWidth,
                count: bad.length, stickingOut: bad.slice(0, 3),
                displays: cells.map(function (td) { return getComputedStyle(td).display; }),
                labels: cells.map(function (td) {
                  return (getComputedStyle(td, '::before').content || '').replace(/^"|"$/g, '');
                }),
                headers: headers,
                keyScroll: key.scrollWidth, keyBox: key.clientWidth
              };
            })()
        """)
        check_true("the ledger view does not scroll sideways either",
                   ledger_narrow["scrollWidth"] <= ledger_narrow["viewport"]
                   and ledger_narrow["count"] == 0)
        check_true("the ledger entries become cards on a phone as well",
                   len(ledger_narrow["displays"]) > 0
                   and all(d in ("flex", "block") for d in ledger_narrow["displays"]))
        check_true("and every card carries its column name",
                   all(l and l in ledger_narrow["headers"] for l in ledger_narrow["labels"]))
        check_true("an unbreakable key wraps inside its row instead of widening the page",
                   ledger_narrow["keyScroll"] <= ledger_narrow["keyBox"] + 1)
        browser.evaluate("document.getElementById('navProxies').click()")
    else:
        print("SKIP  the ledger view on a phone: this deployment has no ledger entries")

    # --- the same page on a desktop viewport --------------------------------
    browser.call("Emulation.clearDeviceMetricsOverride", session=True)
    desktop = browser.evaluate("""
        (function () {
          var cell = document.querySelector('#proxiesBody td');
          var wrap = document.querySelector('#proxiesTable').parentNode;
          return {
            cell: cell ? getComputedStyle(cell).display : null,
            header: getComputedStyle(document.querySelector('#proxiesTable thead')).display,
            label: cell ? getComputedStyle(cell, '::before').content : null,
            menu: getComputedStyle(document.getElementById('menuToggle')).display,
            sidebar: getComputedStyle(document.getElementById('sidebar')).position,
            wrapOverflowX: getComputedStyle(wrap).overflowX,
            docWidth: document.documentElement.scrollWidth,
            viewport: window.innerWidth
          };
        })()
    """)
    if proxy_rows_present:
        check("the table is a table again on a desktop viewport", desktop["cell"], "table-cell")
        check_true("its header row is back", desktop["header"] != "none")
        check("the per-cell labels are gone", desktop["label"], "none")
    check("the drawer button is hidden on a wide viewport", desktop["menu"], "none")
    check_true("the sidebar is back in the layout", desktop["sidebar"] in ("sticky", "static", "relative"))
    check_true("a table too wide for the viewport scrolls inside its wrapper",
               desktop["wrapOverflowX"] in ("auto", "scroll") and desktop["docWidth"] <= desktop["viewport"])

    # --- what a screen reader gets -----------------------------------------
    nodes = browser.call("Accessibility.getFullAXTree", session=True)["nodes"]
    interactive = {"button", "link", "combobox", "textbox", "checkbox", "menuitem"}
    unnamed = []
    names = {}
    for node in nodes:
        role = (node.get("role") or {}).get("value")
        if role not in interactive or node.get("ignored"):
            continue
        name = (node.get("name") or {}).get("value") or ""
        names.setdefault(role, []).append(name)
        if not name.strip():
            unnamed.append(role)
    check_true("every control on the page has an accessible name", not unnamed)
    ok("the accessibility tree is readable (%d control(s) named)" % sum(len(v) for v in names.values()))
    check_true("the sections are reachable as a named navigation",
               browser.evaluate("document.getElementById('sidebar').getAttribute('aria-label')") not in ("", None))

    skip = browser.evaluate("""
        (function () {
          var focusable = document.querySelectorAll('a[href], button, select, input, [tabindex]:not([tabindex="-1"])');
          var first = focusable[0];
          return { first: first ? (first.className || first.tagName) : null,
                   href: first && first.getAttribute ? first.getAttribute('href') : null,
                   target: !!document.getElementById('main') };
        })()
    """)
    check_true("the first thing a keyboard reaches is the skip link",
               "skip-link" in (skip["first"] or ""))
    check("and it points at the main region", skip["href"], "#main")

    # The name of the navigation is a sentence like every other label, so it has to
    # follow the language switch: this one used to be written into the markup.
    english_nav = browser.evaluate("document.getElementById('sidebar').getAttribute('aria-label')")
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'zh';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)
    chinese_nav = browser.evaluate("document.getElementById('sidebar').getAttribute('aria-label')")
    check_true("the navigation's accessible name follows the language switch",
               chinese_nav and chinese_nav != english_nav and "栏目" in chinese_nav)
    browser.evaluate("""
        (function () {
          var select = document.getElementById('langSelect');
          select.value = 'en';
          select.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        })()
    """)

    # --- the disconnect button --------------------------------------------
    browser.evaluate("document.getElementById('navClients').click()")
    if not clients and not proxy_rows:
        print("SKIP  the disconnect button: this deployment has no client to disconnect")
        return
    if not clients:
        print("SKIP  the disconnect button: no client is connected right now")
        return
    target = clients[0]["id"]

    # Declining the confirmation must not reach the server.
    browser.evaluate("window.confirm = function () { return false; }")
    browser.evaluate("""
        (function () {
          var row = document.querySelector('#clientsBody tr');
          row.querySelector('button').click();
          return true;
        })()
    """)
    # The row staying in the DOM would not show a DELETE that was wrongly sent (the
    # table is rebuilt only on the next poll), so ask the server over the next few
    # seconds: a request that was sent would land well inside that window.
    deadline = time.time() + 3
    still_listed = True
    while time.time() < deadline:
        still_listed = target in [c["id"] for c in api(url, token, "/api/clients")["clients"]]
        if not still_listed:
            break
        time.sleep(0.3)
    check_true("declining the confirmation sends nothing", still_listed)

    browser.evaluate("window.confirm = function () { return true; }")
    browser.evaluate("""
        (function () {
          var row = document.querySelector('#clientsBody tr');
          row.querySelector('button').click();
          return true;
        })()
    """)
    # The table is rebuilt from the next poll, so what is waited for is the row of the
    # client that was disconnected, not an empty table: other clients keep their rows.
    gone = False
    deadline = time.time() + 5
    while time.time() < deadline:
        shown = browser.evaluate("""
            Array.prototype.map.call(document.querySelectorAll('#clientsBody tr'),
              function (row) { return row.children[0].textContent.trim(); })
        """)
        if target not in shown:
            gone = True
            break
        time.sleep(0.2)
    check_true("confirming disconnects the client the panel was showing", gone)
    ok("the panel re-polls and drops the row it disconnected")

    remaining = api(url, token, "/api/clients")["clients"]
    check_true("the server no longer lists that client (the client reconnects on its own)",
               target not in [c["id"] for c in remaining])

    if args.audit and os.path.exists(args.audit):
        found = 0
        with open(args.audit, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line or "dashboard_action" not in line:
                    continue
                try:
                    record = json.loads(line)
                except ValueError:
                    continue
                if record.get("client_id") == target:
                    found += 1
        check_true("the disconnect the panel sent is in the audit log", found >= 1)
    else:
        print("SKIP  the audit record of the disconnect: no audit log was given")

    # --- a second deployment, with the optional sections off ----------------
    # The off state is what an operator sees until a section is turned on, and it is
    # drawn by the same rows as the on state, so it is checked on a deployment that
    # really has those sections disabled.
    if args.bare_url:
        browser.navigate(args.bare_url.rstrip("/") + "/")
        browser.wait_for("document.getElementById('authOverlay').hidden === false",
                         label="the token prompt on the ledger-off deployment")
        browser.evaluate("""
            (function () {
              document.getElementById('authInput').value = %s;
              document.getElementById('authForm').dispatchEvent(
                new Event('submit', { cancelable: true, bubbles: true }));
              return true;
            })()
        """ % json.dumps(args.bare_token))
        browser.wait_for("document.getElementById('authOverlay').hidden === true",
                         label="the prompt to close on the ledger-off deployment")
        check_ledger_off(browser, "the bare deployment")
        check_directory_off(browser, "the bare deployment")
    else:
        print("SKIP  the ledger-off rendering: no --bare-url was given")


if __name__ == "__main__":
    sys.exit(main())
