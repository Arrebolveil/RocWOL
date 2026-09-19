package wiki.hubu.rocwol.ui

import android.content.ActivityNotFoundException
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.launch
import wiki.hubu.rocwol.data.journal.JournalCategory
import wiki.hubu.rocwol.data.journal.JournalHealth
import wiki.hubu.rocwol.data.journal.JournalProblem
import wiki.hubu.rocwol.data.journal.JournalRecord
import wiki.hubu.rocwol.data.journal.OperationJournal
import wiki.hubu.rocwol.data.journal.WakeStage
import wiki.hubu.rocwol.data.journal.safeText
import java.time.ZoneId
import java.time.format.DateTimeFormatter

@OptIn(androidx.compose.foundation.layout.ExperimentalLayoutApi::class)
@Composable
fun JournalScreen(journal: OperationJournal, onBack: () -> Unit) {
    BackHandler(onBack = onBack)
    val records by journal.records.collectAsStateWithLifecycle()
    val health by journal.health.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    var categoryName by rememberSaveable { mutableStateOf<String?>(null) }
    val category = JournalCategory.entries.firstOrNull { it.name == categoryName }
    var failuresOnly by rememberSaveable { mutableStateOf(false) }
    var categoryMenu by remember { mutableStateOf(false) }
    var clearConfirmation by rememberSaveable { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var selected by remember { mutableStateOf<JournalRecord?>(null) }
    val zone = ZoneId.systemDefault()
    val groups = remember(records, category, failuresOnly, zone) {
        records.filter { (category == null || it.event.category == category) && (!failuresOnly || it.isFailure) }
            .groupBy { it.timestamp.atZone(zone).toLocalDate() }
    }
    Scaffold(snackbarHost = { SnackbarHost(snackbar) }) { padding ->
        Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
            Column(Modifier.widthIn(max = 840.dp).fillMaxSize()) {
                JournalHeading("操作日志", onBack)
                FlowRow(Modifier.fillMaxWidth().padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Box {
                        OutlinedButton(onClick = { categoryMenu = true }) { Text(category?.label ?: "全部分类") }
                        DropdownMenu(expanded = categoryMenu, onDismissRequest = { categoryMenu = false }) {
                            DropdownMenuItem(text = { Text("全部分类") }, onClick = { categoryName = null; categoryMenu = false })
                            JournalCategory.entries.forEach { value ->
                                DropdownMenuItem(text = { Text(value.label) }, onClick = { categoryName = value.name; categoryMenu = false })
                            }
                        }
                    }
                    FilterChip(selected = failuresOnly, onClick = { failuresOnly = !failuresOnly }, label = { Text("仅失败 / 未知") })
                    TextButton(enabled = !busy && health.loaded, onClick = {
                        scope.launch {
                            busy = true
                            try {
                                val intent = journal.exportShareIntent()
                                if (intent == null) snackbar.showSnackbar("无法导出日志，请检查日志状态或可用存储空间")
                                else try {
                                    context.startActivity(intent)
                                } catch (_: ActivityNotFoundException) {
                                    snackbar.showSnackbar("没有可用于分享文本的应用")
                                } catch (_: SecurityException) {
                                    snackbar.showSnackbar("无法授予日志文件的只读访问权限")
                                }
                            } finally { busy = false }
                        }
                    }) { Text("导出全部") }
                    TextButton(enabled = !busy && health.loaded, onClick = { clearConfirmation = true }) { Text("清空") }
                }
                JournalHealthBanner(health)
                if (busy) LinearProgressIndicator(Modifier.fillMaxWidth())
                LazyColumn(Modifier.fillMaxSize(), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    item {
                        Text("仅保留本机最近 7 天、最多 2000 条记录。自动重复检查会合并；用户操作逐条保留。导出包含服务入口和请求 ID，不包含凭据或设备地址。",
                            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp), style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    if (groups.isEmpty() && health.loaded) item {
                        Text(if (category != null || failuresOnly) "没有符合筛选条件的记录" else "暂无本机操作日志", Modifier.padding(24.dp))
                    }
                    groups.forEach { (date, entries) ->
                        item(key = "date-$date") {
                            Text(date.toString(), Modifier.padding(horizontal = 20.dp, vertical = 8.dp), style = MaterialTheme.typography.titleSmall)
                        }
                        items(entries, key = { it.id }) { entry ->
                            JournalEntryCard(entry, onClick = { selected = entry })
                        }
                    }
                    item { Text("已确认写入的记录可在重启后保留；等待写入的记录可能因进程终止而丢失。",
                        Modifier.padding(16.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                }
            }
        }
    }
    selected?.let { JournalEntryDetails(it, onDismiss = { selected = null }) }
    if (clearConfirmation) AlertDialog(
        onDismissRequest = { clearConfirmation = false },
        title = { Text("清空本机日志？") },
        text = { Text("操作日志和本机唤醒历史将被永久清空。本机身份、管理授权和上次唤醒状态不会改变；已分享给其他应用的副本无法撤回。") },
        confirmButton = {
            TextButton(onClick = {
                clearConfirmation = false
                scope.launch {
                    busy = true
                    try {
                        val cleared = journal.clear()
                        if (cleared) selected = null
                        snackbar.showSnackbar(if (cleared) "本机日志已清空" else "清空失败，请检查日志存储状态")
                    } finally { busy = false }
                }
            }) { Text("确认清空") }
        },
        dismissButton = { TextButton(onClick = { clearConfirmation = false }) { Text("取消") } }
    )
}

@Composable
fun WakeHistoryScreen(journal: OperationJournal, onBack: () -> Unit) {
    BackHandler(onBack = onBack)
    val records by journal.records.collectAsStateWithLifecycle()
    val health by journal.health.collectAsStateWithLifecycle()
    var selected by remember { mutableStateOf<JournalRecord?>(null) }
    val groups = remember(records) {
        records.filter { it.event.category == JournalCategory.WAKE && it.event.requestId != null }
            .groupBy { it.event.requestId!! }
            .map { (requestId, values) -> requestId to values.sortedWith(compareBy<JournalRecord> { it.timestamp }.thenBy { it.id }) }
            .sortedByDescending { (_, values) -> values.maxOf { it.lastTimestamp } }
    }
    Scaffold { padding ->
        Box(Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
            Column(Modifier.widthIn(max = 840.dp).fillMaxSize()) {
                JournalHeading("唤醒历史", onBack)
                JournalHealthBanner(health)
                LazyColumn(Modifier.fillMaxSize(), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    item {
                        Text("仅显示本机保留的请求记录（最近 7 天、最多 2000 条日志），不是服务器完整历史。唤醒包已发送 ≠ 电脑已开机；确认在线仅表示后续探测在线，不证明由该请求唤醒。这里不会联网或重新发送唤醒。",
                            Modifier.padding(16.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    if (groups.isEmpty() && health.loaded) item { Text("暂无本机唤醒历史", Modifier.padding(24.dp)) }
                    groups.forEach { (requestId, timeline) ->
                        item(key = requestId) {
                            WakeRequestCard(requestId, timeline, onSelect = { selected = it })
                        }
                    }
                }
            }
        }
    }
    selected?.let { JournalEntryDetails(it, onDismiss = { selected = null }) }
}

@Composable
private fun JournalHeading(title: String, onBack: () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
        TextButton(onClick = onBack) { Text("返回") }
        Text(title, style = MaterialTheme.typography.headlineSmall, modifier = Modifier.padding(start = 8.dp))
    }
}

@Composable
private fun JournalHealthBanner(health: JournalHealth) {
    if (!health.loaded) LinearProgressIndicator(Modifier.fillMaxWidth())
    if (health.problem != JournalProblem.NONE || health.pendingWrites > 0 || health.droppedRecords > 0) {
        val message = buildString {
            if (health.problem != JournalProblem.NONE) append(health.problem.label)
            if (health.pendingWrites > 0) append(" · 等待写入 ${health.pendingWrites} 条")
            if (health.droppedRecords > 0) append(" · 本次进程未保存 ${health.droppedRecords} 条")
        }.trimStart(' ', '·')
        Text(message, Modifier.padding(horizontal = 16.dp, vertical = 8.dp), style = MaterialTheme.typography.bodySmall,
            color = if (health.problem != JournalProblem.NONE || health.droppedRecords > 0) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun JournalEntryCard(entry: JournalRecord, onClick: () -> Unit) {
    Card(onClick = onClick, modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text("${entry.event.action.label} · ${entry.event.outcome.label}", style = MaterialTheme.typography.titleMedium,
                color = if (entry.isFailure) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface)
            Text("${journalTime(entry)} · ${entry.event.trigger.label}${if (entry.occurrences > 1) " · 合并 ${entry.occurrences} 次" else ""}",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            entry.event.wakeStage?.let { Text(it.label, style = MaterialTheme.typography.bodyMedium) }
            entry.event.error?.let { Text("错误类别：${it.code}", style = MaterialTheme.typography.bodySmall) }
        }
    }
}

@Composable
private fun WakeRequestCard(requestId: String, timeline: List<JournalRecord>, onSelect: (JournalRecord) -> Unit) {
    var expanded by rememberSaveable(requestId) { mutableStateOf(false) }
    var page by rememberSaveable(requestId) { mutableIntStateOf(0) }
    val lastPage = (timeline.size - 1) / 20
    val currentPage = page.coerceAtMost(lastPage)
    val end = timeline.size - currentPage * 20
    val start = (end - 20).coerceAtLeast(0)
    val stage = timeline.filter { it.event.wakeStage != null }.maxWithOrNull(compareBy<JournalRecord> { it.lastTimestamp }.thenBy { it.id })?.event?.wakeStage
    Card(Modifier.fillMaxWidth().padding(horizontal = 16.dp)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(stage?.label ?: "尚无确定结果", style = MaterialTheme.typography.titleMedium)
            SelectionContainer { Text("请求 ID：$requestId", style = MaterialTheme.typography.bodySmall) }
            Text(journalTime(timeline.first()), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (stage == WakeStage.SENT) Text("仅确认唤醒包已发送，未确认电脑开机。", style = MaterialTheme.typography.bodySmall)
            if (stage == WakeStage.SUBMITTED || stage == WakeStage.PENDING || stage == WakeStage.UNKNOWN || stage == null) {
                Text("本机尚未记录最终结果；关闭应用或日志保留期限可能使时间线不完整。", style = MaterialTheme.typography.bodySmall)
            }
            TextButton(onClick = { expanded = !expanded }) { Text(if (expanded) "收起时间线" else "查看时间线（${timeline.size} 条）") }
            if (expanded) {
                timeline.subList(start, end).forEach { entry ->
                    HorizontalDivider()
                    TextButton(onClick = { onSelect(entry) }, modifier = Modifier.fillMaxWidth()) {
                        Column(Modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            Text("${entry.event.wakeStage?.label ?: entry.event.action.label} · ${entry.event.outcome.label}",
                                color = if (entry.isFailure) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface)
                            Text("${journalTime(entry)}${if (entry.occurrences > 1) " · 合并 ${entry.occurrences} 次" else ""}",
                                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                    TextButton(enabled = currentPage < lastPage, onClick = { page = currentPage + 1 }) { Text("更早记录") }
                    Text("${currentPage + 1} / ${lastPage + 1}", Modifier.align(Alignment.CenterVertically), style = MaterialTheme.typography.bodySmall)
                    TextButton(enabled = currentPage > 0, onClick = { page = currentPage - 1 }) { Text("较新记录") }
                }
            }
        }
    }
}

@Composable
private fun JournalEntryDetails(entry: JournalRecord, onDismiss: () -> Unit) {
    val clipboard = LocalClipboardManager.current
    var copied by remember(entry.id) { mutableStateOf(false) }
    val text = remember(entry) { entry.safeText() }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("日志详情") },
        text = { SelectionContainer { Text(text, Modifier.verticalScroll(rememberScrollState()), style = MaterialTheme.typography.bodyMedium) } },
        confirmButton = { TextButton(onClick = onDismiss) { Text("关闭") } },
        dismissButton = { TextButton(onClick = { clipboard.setText(AnnotatedString(text)); copied = true }) { Text(if (copied) "已复制" else "复制脱敏内容") } }
    )
}

private fun journalTime(record: JournalRecord): String =
    DateTimeFormatter.ofPattern("MM-dd HH:mm:ss").withZone(ZoneId.systemDefault()).format(record.timestamp)
