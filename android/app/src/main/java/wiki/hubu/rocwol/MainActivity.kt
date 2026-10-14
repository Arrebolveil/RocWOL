package wiki.hubu.rocwol

import android.os.Bundle
import android.content.Intent
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import kotlinx.coroutines.flow.first
import wiki.hubu.rocwol.data.AppMigration
import wiki.hubu.rocwol.data.ServerProfiles
import wiki.hubu.rocwol.reminders.AuthorizationReminders
import wiki.hubu.rocwol.reminders.ReminderPolicy
import wiki.hubu.rocwol.ui.RocApp
import wiki.hubu.rocwol.ui.WolViewModel

class MainActivity : ComponentActivity() {
    private lateinit var model: WolViewModel
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        model = ViewModelProvider(this)[WolViewModel::class.java]
        setContent { RocApp(this, model) }
        navigateNotification(intent)
    }
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        navigateNotification(intent)
    }
    private fun navigateNotification(intent: Intent?) {
        if (intent == null) return
        lifecycleScope.launch {
            try {
                AppMigration.ensure(applicationContext)
            } catch (error: Exception) {
                if (error is CancellationException) throw error
                // Startup exposes the retryable error. Keep this navigation suspended until
                // the user's explicit startup retry succeeds, without reading partial data.
                model.state.first { it.ready }
                AppMigration.ensure(applicationContext)
            }
            val profileId = ReminderPolicy.navigationProfileId(
                intent.data?.toString(),
                intent.getStringExtra(AuthorizationReminders.PROFILE_ID_EXTRA),
                intent.getBooleanExtra(AuthorizationReminders.OPEN_MANAGEMENT_EXTRA, false)
            ) ?: return@launch
            val profile = ServerProfiles.get(applicationContext).get(profileId)
            if (profile == null || profile.removalMode != null) {
                Toast.makeText(this@MainActivity, "服务器已移除", Toast.LENGTH_LONG).show()
                return@launch
            }
            // ViewModel queues navigation through authentication/revocation and revalidates
            // the profile before activation. A notification never authorizes or opens a browser.
            model.notification(profileId)
        }
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
