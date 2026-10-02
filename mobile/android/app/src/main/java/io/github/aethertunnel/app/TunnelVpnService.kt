package io.github.aethertunnel.app

import android.content.Intent
import android.net.VpnService
import android.os.ParcelFileDescriptor
import android.util.Log
import io.github.aethertunnel.mobile.Mobile
import io.github.aethertunnel.mobile.PlatformVPN
import java.io.IOException

// The platform half of the layer-3 tunnel. The service owns the tun interface;
// the Go client owns the packets. It implements PlatformVPN: the Go client calls
// OpenTun at the moment the session is up and the server's address assignment is
// known — the only moment a platform interface can be configured correctly — and
// calls protectSocket for every socket it opens to the server, so the tunnel's
// own traffic never loops into the interface it feeds.
class TunnelVpnService : VpnService(), PlatformVPN {

    private var tun: ParcelFileDescriptor? = null
    private var worker: Thread? = null
    private var fullTunnel = false

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        var config = intent?.getStringExtra(EXTRA_CONFIG)
        if (config.isNullOrEmpty()) {
            // CI drives the service directly: it drops the configuration into the
            // app's own files directory with run-as and starts the service with
            // no extras. Nothing else reads this path.
            config = try {
                getFileStreamPath(CONFIG_FILE).takeIf { it.exists() }?.readText()
            } catch (_: Exception) {
                null
            }
        }
        if (config.isNullOrEmpty() || worker != null) {
            Log.i(TAG, "service not started: config empty=${config.isNullOrEmpty()} worker running=${worker != null}")
            return START_NOT_STICKY
        }
        Log.i(TAG, "service starting: config ${config.length} chars, fullTunnel=$fullTunnel")
        fullTunnel = intent?.getBooleanExtra(EXTRA_FULL_TUNNEL, false) ?: false
        worker = Thread {
            try {
                // Blocks until Mobile.stop() is called or the client gives up on a
                // fatal error; OpenTun below runs from inside this call.
                Mobile.runVPN(config, this)
                Log.i(TAG, "vpn client stopped")
            } catch (e: Throwable) {
                Log.e(TAG, "vpn client failed", e)
            } finally {
                stopSelf()
            }
        }
        worker?.start()
        return START_NOT_STICKY
    }

    override fun openTun(mtu: Int, address: String, prefix: Int, subnet: String): Int {
        Log.i(
            TAG,
            if (fullTunnel) "establishing the VPN interface $address/$prefix, full device routing"
            else "establishing the VPN interface $address/$prefix, route $subnet"
        )
        val builder = Builder()
            .setSession("AetherTunnel")
            .setMtu(mtu)
            .addAddress(address, prefix)
        if (fullTunnel) {
            // Capture everything and give the phone a resolver that is reachable
            // through the tunnel; the client's own sockets stay outside the routes
            // because every one of them runs through protectSocket below.
            builder.addRoute("0.0.0.0", 0)
            builder.addRoute("::", 0)
            builder.addDnsServer(DNS_SERVER)
        } else {
            // Route only the tunnel's own subnet: nothing else is captured, so the
            // tunnel's sockets cannot loop into the interface they feed.
            builder.addRoute(subnet, prefix)
        }
        val vpn = builder.establish() ?: throw IOException("the system refused to establish the VPN")
        tun = vpn
        return vpn.detachFd()
    }

    override fun protectSocket(fd: Int) {
        // VpnService.protect returns false when no VPN is established, which is
        // the normal state before the first session — so no bookkeeping here.
        protect(fd)
    }

    override fun onDestroy() {
        Mobile.stop()
        worker = null
        try {
            tun?.close()
        } catch (_: Exception) {
        }
        tun = null
        super.onDestroy()
    }

    companion object {
        private const val TAG = "AetherTunnel"
        // The resolver the phone uses in full-tunnel mode; its queries travel
        // through the tunnel like any other packet.
        private const val DNS_SERVER = "1.1.1.1"
        const val EXTRA_CONFIG = "config"
        const val EXTRA_FULL_TUNNEL = "full_tunnel"
        private const val CONFIG_FILE = "client.toml"
    }
}
