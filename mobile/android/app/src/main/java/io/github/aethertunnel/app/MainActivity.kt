package io.github.aethertunnel.app

import android.app.Activity
import android.graphics.Typeface
import android.os.Bundle
import android.view.Gravity
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import io.github.aethertunnel.mobile.Mobile

// A minimal console for the embedded client: edit a client configuration, start
// it on a worker thread, stop it with a button. Everything else — the reconnect
// loop, the visitors, the tunnel itself — is the Go code in pkg/clientlib behind
// the Mobile binding; the app adds nothing but a UI around it. Logs go to the
// screen here and to logcat (gomobile routes the Go logger's stderr there).
class MainActivity : Activity() {

    private lateinit var logView: TextView

    @Volatile
    private var running = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val pad = (16 * resources.displayMetrics.density).toInt()

        val config = EditText(this).apply {
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
                        Mobile.run(config.text.toString())
                        logLine("client stopped")
                    } catch (e: Throwable) {
                        logLine("error: ${e.message ?: e.javaClass.simpleName}")
                    } finally {
                        running = false
                    }
                }.start()
            }
        }

        val stop = Button(this).apply {
            text = "Stop"
            setOnClickListener {
                logLine("stop requested")
                Mobile.stop()
            }
        }

        val buttons = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            addView(start, weight(1f))
            addView(stop, weight(1f))
        }

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
            addView(config)
            addView(
                buttons,
                LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.MATCH_PARENT,
                    LinearLayout.LayoutParams.WRAP_CONTENT
                )
            )
            addView(
                log,
                LinearLayout.LayoutParams(
                    LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f
                )
            )
        }
        setContentView(root)
    }

    private fun weight(weight: Float) =
        LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, weight)

    private fun logLine(line: String) = runOnUiThread {
        logView.append(if (logView.length() == 0) line else "\n$line")
    }

    private companion object {
        const val SAMPLE = "[client]\n" +
            "server_addr = \"127.0.0.1:7001\"\n" +
            "auth_token = \"change-me-16-random-characters\"\n"
    }
}
