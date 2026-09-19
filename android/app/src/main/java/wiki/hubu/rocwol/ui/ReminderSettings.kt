package wiki.hubu.rocwol.ui

import android.Manifest
import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.selection.toggleable
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import wiki.hubu.rocwol.reminders.AuthorizationReminders
import wiki.hubu.rocwol.reminders.AuthorizationReminders.NotificationAvailability

@Composable
fun ReminderSettings(activity: Activity, reminders: AuthorizationReminders) {
    val enabled by reminders.enabled.collectAsStateWithLifecycle()
    val scope = rememberCoroutineScope()
    val lifecycleOwner = LocalLifecycleOwner.current
    var availability by remember(reminders) { mutableStateOf(reminders.notificationAvailability()) }
    var permissionDenied by rememberSaveable { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }

    fun saveEnabled(value: Boolean) {
        busy = true
        error = null
        scope.launch {
            try {
                reminders.setEnabled(value)
                availability = reminders.notificationAvailability()
            } catch (e: CancellationException) {
                throw e
            } catch (_: Exception) {
                error = "提醒设置未能完整保存，请稍后重试。其他功能不受影响。"
            } finally {
                busy = false
            }
        }
    }

    val permissionLauncher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        availability = reminders.notificationAvailability()
        permissionDenied = !granted
        if (granted) saveEnabled(true) else busy = false
    }

    DisposableEffect(lifecycleOwner, reminders) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_RESUME) {
                val current = reminders.notificationAvailability()
                val becameAvailable = current == NotificationAvailability.AVAILABLE && availability != current
                availability = current
                if (becameAvailable && reminders.enabled.value) saveEnabled(true)
            }
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    val toggle: (Boolean) -> Unit = { value ->
        permissionDenied = false
        error = null
        if (value && Build.VERSION.SDK_INT >= 33 &&
            reminders.notificationAvailability() == NotificationAvailability.PERMISSION_REQUIRED
        ) {
            // Permission is requested only in response to this explicit enable action.
            busy = true
            permissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        } else {
            saveEnabled(value)
        }
    }

    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("授权安全提醒", style = MaterialTheme.typography.titleLarge)
            Row(
                Modifier.fillMaxWidth().toggleable(
                    value = enabled, enabled = !busy, role = Role.Switch, onValueChange = toggle
                ).padding(vertical = 4.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(16.dp)
            ) {
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text("本地授权提醒", style = MaterialTheme.typography.titleMedium)
                    Text(
                        if (enabled) "已开启提醒偏好" else "默认关闭，由你选择开启",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant
                    )
                }
                Switch(checked = enabled, onCheckedChange = null, enabled = !busy)
            }
            Text(
                "每次授权最多提醒一次：永久授权约 24 小时后提醒；允许所有设备且剩余时间超过 5 分钟时，约 5 分钟后提醒。后者优先。短时授权可能不会提醒。",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )
            Text(
                "系统可能延迟或限制通知，这不是精确的到期闹钟。提醒只依据上次确认的授权，离线时无法确定是否仍开放；请打开 App 核实或取消。不会自动联网、授权或撤销。开关不会改变现有授权。",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )
            val guidance = when {
                permissionDenied -> "未获得通知权限，提醒未开启。唤醒和管理等其他功能仍可正常使用；需要时可再次开启或前往系统设置。"
                enabled && availability == NotificationAvailability.PERMISSION_REQUIRED -> "系统未授予通知权限，当前无法显示提醒。可在系统设置中允许通知，或关闭后重新开启此开关。"
                availability == NotificationAvailability.CHANNEL_BLOCKED -> "“管理授权安全提醒”通知类别已被系统关闭。当前无法显示提醒，请在系统设置中开启该类别。"
                availability == NotificationAvailability.APP_BLOCKED -> "此 App 的通知已被系统关闭，当前无法显示提醒。可前往系统设置允许通知。"
                else -> null
            }
            guidance?.let {
                Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
            }
            error?.let {
                Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.error)
            }
            TextButton(onClick = {
                val settings = if (availability == NotificationAvailability.CHANNEL_BLOCKED) {
                    Intent(Settings.ACTION_CHANNEL_NOTIFICATION_SETTINGS)
                        .putExtra(Settings.EXTRA_APP_PACKAGE, activity.packageName)
                        .putExtra(Settings.EXTRA_CHANNEL_ID, AuthorizationReminders.CHANNEL_ID)
                } else {
                    Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                        .putExtra(Settings.EXTRA_APP_PACKAGE, activity.packageName)
                }
                try {
                    activity.startActivity(settings)
                } catch (_: ActivityNotFoundException) {
                    try {
                        activity.startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                            Uri.parse("package:${activity.packageName}")))
                    } catch (_: ActivityNotFoundException) {
                        error = "无法打开系统设置，请在系统设置中找到网络唤醒并检查通知权限。"
                    }
                }
            }) {
                Text("系统通知设置")
            }
        }
    }
}
