package wiki.hubu.rocwol.ui

import android.app.Activity
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Card
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.runtime.key
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.compose.ui.window.DialogWindowProvider
import androidx.core.view.WindowCompat

private enum class Utility { SERVERS, JOURNAL, HISTORY, DIAGNOSTICS, DEVICES, ABOUT }

@Composable
internal fun SettingsUtilities(activity: Activity, model: WolViewModel, state: ScreenState) {
    var selected by rememberSaveable { mutableStateOf<Utility?>(null) }
    val fixedRepository = remember(selected) { model.repository }
    val fixedContext = remember(selected) { fixedRepository?.let { repo -> runCatching { repo.operationContext().copy(selectionGeneration = state.selectionGeneration) }.getOrNull() } }
    LaunchedEffect(state.activeProfileId) {
        if (selected == Utility.DIAGNOSTICS || selected == Utility.DEVICES) selected = null
    }
    Card(Modifier.fillMaxWidth()) {
        Column {
            Text("工具与记录", style = MaterialTheme.typography.titleMedium, modifier = Modifier.padding(20.dp))
            UtilityEntry("管理服务器", "添加、重命名、验证线路与删除档案", enabled = state.ready && !state.switchingLocked) { selected = Utility.SERVERS }
            UtilityEntry("操作日志", "本机保存，可筛选、清空及脱敏导出") { selected = Utility.JOURNAL }
            UtilityEntry("唤醒历史", "按请求查看提交、发包和在线确认") { selected = Utility.HISTORY }
            UtilityEntry("连接诊断", if (state.paired) "只读检查当前线路，不发送唤醒包" else "配对后可检查服务器与路由器",
                enabled = state.paired && state.selectedTargetId != null && state.origin.isNotEmpty() && !state.busy && !state.switching) { selected = Utility.DIAGNOSTICS }
            UtilityEntry("配对设备", if (state.paired) "专用管理权限与系统认证保护" else "配对后可查看本机管理权限",
                enabled = state.paired && state.origin.isNotEmpty() && !state.busy && !state.switching) { selected = Utility.DEVICES }
            HorizontalDivider()
            UtilityEntry("关于网络唤醒", "版本、客户端协议与发布页面") { selected = Utility.ABOUT }
        }
    }
    ReminderSettings(activity, model.reminders)
    selected?.let { destination ->
        UtilityWindow(onBack = { if (!model.state.value.switchingLocked) selected = null }) {
            when (destination) {
                Utility.SERVERS -> ServerProfilesScreen(model) { selected = null }
                Utility.JOURNAL -> JournalScreen(model.journal, onBack = { selected = null }, activeProfileId = state.activeProfileId, selectedTargetId = state.selectedTargetId, profiles = state.profiles)
                Utility.HISTORY -> WakeHistoryScreen(model.journal, onBack = { selected = null }, activeProfileId = state.activeProfileId, selectedTargetId = state.selectedTargetId, profiles = state.profiles)
                Utility.DIAGNOSTICS -> if (fixedRepository != null && fixedContext?.targetId != null) DiagnosticsScreen(fixedRepository, fixedContext,
                    { report -> model.recordDiagnostic(fixedContext, report) }) { selected = null }
                Utility.DEVICES -> fixedRepository?.let { repo ->
                    DeviceAdministrationScreen(activity, repo, {
                        model.identityRevoked(repo.profileId)
                        selected = null
                    }, model::lockSwitching) { selected = null }
                }
                Utility.ABOUT -> AboutScreen { selected = null }
            }
        }
    }
}

@Composable
private fun UtilityEntry(title: String, description: String, enabled: Boolean = true, onClick: () -> Unit) {
    ListItem(
        headlineContent = { Text(title) }, supportingContent = { Text(description) },
        colors = ListItemDefaults.colors(containerColor = Color.Transparent,
            headlineColor = if (enabled) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant),
        modifier = Modifier.clickable(enabled = enabled, role = Role.Button, onClick = onClick)
    )
}

@Composable
internal fun WakeHistoryAction(model: WolViewModel) {
    var open by rememberSaveable { mutableStateOf(false) }
    OutlinedButton(onClick = { open = true }, modifier = Modifier.fillMaxWidth()) { Text("查看唤醒历史") }
    if (open) UtilityWindow(onBack = { open = false }) { WakeHistoryScreen(model.journal, onBack = { open = false }, activeProfileId = model.state.value.activeProfileId, selectedTargetId = model.state.value.selectedTargetId, profiles = model.state.value.profiles) }
}

@Composable
internal fun UtilityWindow(onBack: () -> Unit, content: @Composable () -> Unit) {
    Dialog(onDismissRequest = onBack, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        val view = LocalView.current
        val window = (view.parent as? DialogWindowProvider)?.window
        val light = MaterialTheme.colorScheme.surface.luminance() > 0.179f
        SideEffect {
            if (window != null) WindowCompat.getInsetsController(window, view).apply {
                isAppearanceLightStatusBars = light
                isAppearanceLightNavigationBars = light
            }
        }
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.surface, content = content)
    }
}
