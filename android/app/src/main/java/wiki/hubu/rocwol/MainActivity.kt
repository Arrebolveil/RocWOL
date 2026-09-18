package wiki.hubu.rocwol

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.lifecycle.ViewModelProvider
import wiki.hubu.rocwol.ui.RocApp
import wiki.hubu.rocwol.ui.WolViewModel

class MainActivity : ComponentActivity() {
    private lateinit var model: WolViewModel
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        model = ViewModelProvider(this)[WolViewModel::class.java]
        setContent { RocApp(this, model) }
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
