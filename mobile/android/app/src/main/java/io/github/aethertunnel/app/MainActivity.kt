package io.github.aethertunnel.app

import android.app.Activity
import android.content.Intent
import android.graphics.Typeface
import android.net.VpnService
import android.os.Bundle
import android.view.Gravity
import android.widget.Button
import android.widget.CheckBox
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import io.github.aethertunnel.mobile.Mobile

// A minimal console for the embedded client: edit a client configuration, start
// it on a worker thread, stop it with a button — and, with the VPN button, hand
// the layer-3 interface to the same client through TunnelVpnService. Everything
// else — the reconnect loop, the visitors, the tunnel itself — is the Go code in
// pkg/clientlib behind the Mobile binding; the app adds nothing but a UI around
// it. Logs go to the screen here and to logcat (gomobile routes the Go logger's
// stderr there); the VPN service logs under the "AetherTunnel" tag.
class MainActivity : Activity() {

    private lateinit var logView: TextView
    private lateinit var configView: EditText
    private lateinit var fullTunnelBox: CheckBox

    @Volatile
    private var running = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Warming the binding: Stop is a harmless no-op when nothing runs, but it
        // forces libgojni to load here — so an install with a broken or missing
        // native library crashes on launch instead of failing later, invisibly.
        try {
            Mobile.stop()
        } catch (e: Throwable) {
            throw RuntimeException("the Go runtime failed to load", e)
        }

        val pad = (16 * resources.displayMetrics.density).toInt()

        configView = EditText(this).apply {
            hint = "client configuration (TOML)"
            setText(SAMPLE)
            minLines = 6
            gravity = Gravity.TOP
            setTextIsSelectable(true)
        }

        logView = TextView(this).apply {
            typeface = Typeface.MONOSPACE
            setTextIsSelectable(true)
        }
        val log = ScrollView(this).apply {
            addView(logView)
        }

        val start = Button(this).apply {
            text = "Start"
            setOnClickListener {
                if (running) return@setOnClickListener
                running = true
                logLine("starting the tunnel")
                Thread {
                    try {
                        Mobile.run(configView.text.toString())
                        logLine("client stopped")
                    } catch (e: Throwable) {
                        logLine("error: ${e.message ?: e.javaClass.simpleName}")
                    } finally {
                        running = false
                    }
                }.start()
            }
        }

        val vpn = Button(this).apply {
            text = "VPN"
            setOnClickListener {
                if (running) return@setOnClickListener
                // The system shows its own consent dialog the first time; the VPN
                // service starts only after it accepts.
                val consent = VpnService.prepare(this@MainActivity)
                if (consent != null) {
                    startActivityForResult(consent, REQUEST_VPN)
                } else {
                    startVpn()
                }
            }
        }

        val stop = Button(this).apply {
            text = "Stop"
            setOnClickListener {
                logLine("stop requested")
                stopService(Intent(this@MainActivity, TunnelVpnService::class.java))
                Mobile.stop()
            }
        }

        val buttons = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            addView(start, weight(1f))
            addView(vpn, weight(1f))
            addView(stop, weight(1f))
        }

        fullTunnelBox = CheckBox(this).apply {
            text = "VPN: capture all device traffic"
            isChecked = false
        }

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
            addView(configView)
            addView(
                buttons,
                LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.MATCH_PARENT,
                    LinearLayout.LayoutParams.WRAP_CONTENT
                )
            )
            addView(fullTunnelBox)
            addView(
                log,
                LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f
                )
            )
        }
        setContentView(root)
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == REQUEST_VPN) {
            if (resultCode == RESULT_OK) {
                startVpn()
            } else {
                logLine("VPN consent refused")
            }
        }
    }

    private fun startVpn() {
        running = true
        val full = fullTunnelBox.isChecked
        logLine(if (full) "starting the VPN service (full tunnel)" else "starting the VPN service (subnet)")
        startService(
            Intent(this, TunnelVpnService::class.java)
                .putExtra(TunnelVpnService.EXTRA_CONFIG, configView.text.toString())
                .putExtra(TunnelVpnService.EXTRA_FULL_TUNNEL, full)
        )
    }

    private fun weight(weight: Float) =
        LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, weight)

    private fun logLine(line: String) = runOnUiThread {
        logView.append(if (logView.length() == 0) line else "\n$line")
    }

    private companion object {
        const val REQUEST_VPN = 41
        const val SAMPLE = "[client]\n" +
            "server_addr = \"127.0.0.1:7001\"\n" +
            "auth_token = \"change-me-16-random-characters\"\n"
    }
}
