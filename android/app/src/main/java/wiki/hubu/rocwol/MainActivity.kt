package wiki.hubu.rocwol

import android.os.Bundle
import android.content.Intent
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.setValue
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.lifecycle.ViewModelProvider
import wiki.hubu.rocwol.ui.RocApp
import wiki.hubu.rocwol.ui.WolViewModel

class MainActivity : ComponentActivity() {
    private lateinit var model: WolViewModel
    private var managementRequests by mutableIntStateOf(0)
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        model = ViewModelProvider(this)[WolViewModel::class.java]
        setContent { RocApp(this, model, managementRequests) }
    }
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (intent.getBooleanExtra("open_management", false)) managementRequests++
    }
    override fun onStart() {
        super.onStart()
        model.foreground(true)
    }
    override fun onStop() {
        model.foreground(false)
        super.onStop()
    }
}
