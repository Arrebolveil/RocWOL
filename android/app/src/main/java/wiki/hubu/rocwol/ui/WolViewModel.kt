package wiki.hubu.rocwol.ui

import android.app.Activity
import android.app.Application
import android.content.Intent
import android.net.Uri
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.*
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.*
import wiki.hubu.rocwol.data.*
import wiki.hubu.rocwol.data.Target
import wiki.hubu.rocwol.data.journal.*
import wiki.hubu.rocwol.reminders.AuthorizationReminders
import wiki.hubu.rocwol.security.*
import java.time.Instant

 data class ScreenState(
    val ready: Boolean = false,
    val profiles: List<ServerProfile> = emptyList(),
    val activeProfileId: String? = null,
    val selectedTargetId: String? = null,
    val selectionGeneration: Long = 0,
    val switching: Boolean = false,
    val switchingLocked: Boolean = false,
    val paired: Boolean = false,
    val deviceName: String = "我的手机",
    val origin: String = "",
    val busy: Boolean = false,
    val managementRefreshing: Boolean = false,
    val server: String = "尚未检测",
    val targets: List<TargetInfo> = emptyList(),
    val targetsCached: Boolean = false,
    val targetsAvailable: Boolean = false,
    val managementEnabled: Boolean = true,
    val target: Target? = null,
    val wake: WakeRecord? = null,
    val resultText: String = "尚无唤醒请求",
    val message: String? = null,
    val waiting: Boolean = false,
    val wasOnline: Boolean = false,
    val waitUntil: Instant? = null,
    val confirmNewWake: Boolean = false,
    val confirmForget: Boolean = false,
    val removalProfileId: String? = null,
    val removalMode: String? = null,
    val confirmLocalRemoval: Boolean = false,
    val sessionMinutes: Int = Repository.DEFAULT_SESSION_MINUTES,
    val autoOpenBrowser: Boolean = true,
    val allowAllDevices: Boolean = false,
    val warnAllDevices: Boolean = true,
    val management: ManagementAuthorization? = null,
    val managementKnown: Boolean = false,
    val managementPending: Boolean = false,
    val managementNavigation: Int = 0,
    val authorizationMarkers: Map<String, String> = emptyMap()
) {
    val activeProfile: ServerProfile? get() = profiles.firstOrNull { it.profileId == activeProfileId }
}

class WolViewModel(application: Application) : AndroidViewModel(application) {
    private val profiles = ServerProfiles.get(application)
    private val repositories = ServerRepositories.get(application)
    val reminders get() = AuthorizationReminders.get(getApplication())
    lateinit var journal: OperationJournal
        private set
    internal var repository: Repository? = null
        private set
    private val mutable = MutableStateFlow(ScreenState())
    val state = mutable.asStateFlow()
    private val noticeChannel = Channel<String>(Channel.CONFLATED)
    val notices = noticeChannel.receiveAsFlow()
    private val readJobs = mutableSetOf<Job>()
    private val effects = mutableMapOf<Job, String>()
    private val cachedTargets = mutableMapOf<String, List<TargetInfo>>()
    private var foreground = false
    private var queuedManagementProfile: String? = null
    private var poll: Job? = null
    private var startupJob: Job? = null

    private data class Session(val repository: Repository, val context: OperationContext, var managementEnabled: Boolean? = null)
    private fun capture(targetId: String? = state.value.selectedTargetId): Session? {
        val repo = repository ?: return null
        if (!state.value.ready || state.value.switching || repo.profile.removalMode != null) return null
        return Session(repo, repo.operationContext(targetId).copy(selectionGeneration = state.value.selectionGeneration))
    }
    private fun Session.publish(transform: (ScreenState) -> ScreenState) {
        mutable.update { if (it.activeProfileId == context.profileId && it.selectionGeneration == context.selectionGeneration) transform(it) else it }
    }
    init { startup() }
    fun startup() {
        if (startupJob?.isActive == true) return
        startupJob = viewModelScope.launch {
            try {
                AppMigration.ensure(getApplication())
                profiles.load()
                journal = OperationJournal.ready(getApplication())
                reminders
                val markers = profiles.profiles.value.filter { it.removalMode == null }.associate { profile ->
                    val repo = repositories.get(profile.profileId)
                    profile.profileId to if (repo.managementPending && !repo.managementKnown) "授权未知" else if (repo.management != null) "管理授权开放" else ""
                }
                mutable.update { it.copy(ready = true, profiles = profiles.profiles.value, authorizationMarkers = markers) }
                activate(profiles.activeProfileId.value)
                launch { profiles.profiles.collect { list -> mutable.update { it.copy(profiles = list) } } }
                processNotification()
            } catch (_: CancellationException) { throw CancellationException() }
            catch (_: Exception) { mutable.update { it.copy(ready = false, message = "本机存储或升级未完成；未连接服务器。请重试升级。") } }
        }
    }
    fun foreground(value: Boolean) {
        foreground = value
        if (!value) { poll?.cancel(); return }
        if (state.value.ready && state.value.paired && state.value.origin.isNotEmpty() && !state.value.busy) refresh(false)
    }
    fun dismissMessage() { mutable.update { it.copy(message = null) } }
    fun dismissConfirmation() { mutable.update { it.copy(confirmNewWake = false, confirmForget = false, confirmLocalRemoval = false) } }
    fun requestForget() { mutable.update { it.copy(confirmForget = true) } }
    fun forget() { state.value.activeProfileId?.let { removeProfile(it, "identity") } }

    private suspend fun stopReads() {
        mutable.update { it.copy(switching = true) }
        cancelReads()
    }
    private suspend fun cancelReads() {
        val current = currentCoroutineContext()[Job]
        val jobs = readJobs.filter { it !== current }
        jobs.forEach { it.cancel() }
        jobs.forEach { it.join() }
        ConnectionDiagnostics.get(getApplication()).cancelAndJoin()
        poll = null
    }
    private suspend fun activate(id: String?) {
        stopReads()
        val generation = state.value.selectionGeneration + 1
        val repo = id?.let { repositories.get(it) }
        repository = repo
        if (repo != null) cachedTargets[repo.profileId] = repo.cachedTargets
        val previous = state.value
        val markers = previous.authorizationMarkers.filterKeys { key -> profiles.get(key) != null }.toMutableMap()
        if (repo != null) markers[repo.profileId] = if (repo.managementPending && !repo.managementKnown) "授权未知" else if (repo.management != null) "管理授权开放" else ""
        val targetId = repo?.profile?.selectedTargetId
        val wake = targetId?.let { repo?.rememberedWake(it) }
        mutable.value = ScreenState(ready = true, profiles = profiles.profiles.value, activeProfileId = id,
            selectionGeneration = generation, selectedTargetId = targetId,
            origin = repo?.profile?.selectedOrigin.orEmpty(), paired = repo?.deviceId != null,
            deviceName = repo?.displayName ?: "我的手机", wake = wake,
            busy = id != null && effects.values.any { it == id },
            resultText = wake?.let(::resultLabel) ?: "尚无唤醒请求",
            targets = id?.let { cachedTargets[it] }.orEmpty(), targetsCached = id?.let { cachedTargets[it] } != null,
            sessionMinutes = repo?.sessionMinutes ?: Repository.DEFAULT_SESSION_MINUTES,
            autoOpenBrowser = repo?.autoOpenBrowser ?: true, allowAllDevices = repo?.allowAllDevices ?: false,
            warnAllDevices = repo?.warnAllDevices ?: true, management = repo?.management,
            managementKnown = repo?.managementKnown ?: false, managementPending = repo?.managementPending ?: false,
            managementNavigation = previous.managementNavigation, authorizationMarkers = markers)
        if (foreground && !state.value.busy && state.value.paired && state.value.origin.isNotEmpty()) refresh(false)
        processNotification()
    }
    fun selectProfile(id: String?) {
        if (state.value.switchingLocked || state.value.switching || id == state.value.activeProfileId) return
        mutable.update { it.copy(switching = true) }
        viewModelScope.launch {
            try { profiles.selectActive(id); activate(id) }
            catch (_: Exception) { mutable.update { it.copy(switching = false, message = "服务器选择未保存，请重试。") } }
        }
    }
    fun notification(profileId: String) { queuedManagementProfile = profileId; viewModelScope.launch { processNotification() } }
    private suspend fun processNotification() {
        if (!state.value.ready || state.value.switchingLocked || state.value.switching) return
        val id = queuedManagementProfile ?: return
        queuedManagementProfile = null
        try {
            val profile = profiles.get(id)
            if (profile == null || profile.removalMode != null) { mutable.update { it.copy(message = "服务器已移除") }; return }
            profiles.selectActive(id)
            activate(id)
            mutable.update { it.copy(managementNavigation = it.managementNavigation + 1) }
        } catch (_: Exception) {
            mutable.update { it.copy(switching = false, message = "未能打开提醒对应的服务器，请重试。") }
        }
    }
    fun selectTarget(id: String) {
        if (state.value.switchingLocked || state.value.switching || id == state.value.selectedTargetId) return
        val repo = repository ?: return
        if (state.value.targets.none { it.targetId == id }) return
        mutable.update { it.copy(switching = true) }
        viewModelScope.launch {
            try {
                stopReads()
                profiles.selectTarget(repo.profileId, id)
                mutable.update { it.copy(switching = false, selectionGeneration = it.selectionGeneration + 1,
                    selectedTargetId = id, target = null, wake = repo.rememberedWake(id), waiting = false,
                    resultText = repo.rememberedWake(id)?.let(::resultLabel) ?: "尚无唤醒请求", busy = effects.values.any { profile -> profile == repo.profileId }) }
                refresh(false)
            } catch (_: Exception) { mutable.update { it.copy(switching = false, message = "电脑选择未保存") } }
            finally { processNotification() }
        }
    }
    fun selectOrigin(origin: String) {
        val repo = repository ?: return
        val profileId = repo.profileId
        if (state.value.switchingLocked || state.value.switching || origin == repo.profile.selectedOrigin) return
        mutable.update { it.copy(switching = true) }
        viewModelScope.launch {
            try {
                stopReads()
                effects.filterValues { it == profileId }.keys.toList().forEach { it.join() }
                repo.withOperation { repo.selectOrigin(origin) }
                activate(profileId)
                noticeChannel.trySend("线路已切换，未重发唤醒请求")
            } catch (e: Exception) {
                mutable.update { it.copy(switching = false, message = errorMessage(e)) }
            } finally { processNotification() }
        }
    }
    fun addServer(name: String, origin: String, complete: (String?) -> Unit) = profileEdit(complete) {
        val saved = profiles.add(name, origin)
        if (!state.value.switchingLocked) { profiles.selectActive(saved.profileId); activate(saved.profileId) }
    }
    fun renameServer(id: String, name: String, complete: (String?) -> Unit) = profileEdit(complete) { profiles.rename(id, name) }
    fun addOrigin(id: String, origin: String, complete: (String?) -> Unit) = profileEdit(complete) {
        profiles.addOrigin(id, origin)
        if (state.value.activeProfileId == id && !state.value.switchingLocked) activate(id)
    }
    fun removeOrigin(id: String, origin: String, complete: (String?) -> Unit) = profileEdit(complete) { profiles.removeOrigin(id, origin) }
    private fun profileEdit(complete: (String?) -> Unit, block: suspend () -> Unit) {
        viewModelScope.launch { try { block(); complete(null) } catch (e: Exception) { complete(errorMessage(e)) } }
    }
    fun removeProfile(id: String, mode: String = "profile", localOnly: Boolean = false) {
        if (state.value.switchingLocked || state.value.switching) return
        mutable.update { it.copy(switching = true) }
        viewModelScope.launch {
            try {
                stopReads()
                effects.filterValues { it == id }.keys.toList().forEach { it.join() }
                val profile = requireNotNull(profiles.get(id))
                if (profile.removalMode == null) {
                    val repo = repositories.get(id)
                    if (!localOnly && (repo.deviceId != null || repo.managementPending)) {
                        try { repo.withOperation { repo.endManagement() } }
                        catch (_: Exception) {
                            mutable.update { it.copy(switching = false, confirmForget = false, confirmLocalRemoval = true, removalProfileId = id, removalMode = mode) }
                            return@launch
                        }
                    }
                }
                profiles.remove(id, mode)
                cachedTargets.remove(id)
                journal.record(JournalEvent(JournalAction.IDENTITY_FORGET, JournalOutcome.SUCCESS, context = OperationContext(id, "")))
                mutable.update { it.copy(confirmForget = false, confirmLocalRemoval = false) }
                activate(profiles.activeProfileId.value)
            } catch (_: Exception) {
                mutable.update { it.copy(switching = false, message = "本机移除未完成；身份可能已关闭，请重试清理。") }
            } finally { processNotification() }
        }
    }
    fun confirmLocalRemoval() { state.value.removalProfileId?.let { removeProfile(it, state.value.removalMode ?: "profile", true) } }

    private fun runOperation(readOnly: Boolean = false, authentication: Boolean = false, block: suspend Session.() -> Unit) {
        val session = try { capture() } catch (e: Exception) { mutable.update { it.copy(message = errorMessage(e)) }; null } ?: return
        if (state.value.busy || state.value.switchingLocked || effects.values.any { it == session.context.profileId }) return
        if (readOnly && readJobs.any { it.isActive }) return
        if (authentication) mutable.update { it.copy(switchingLocked = true) }
        val job = viewModelScope.launch(start = CoroutineStart.LAZY) {
            session.publish { it.copy(busy = if (readOnly) it.busy else true, switchingLocked = it.switchingLocked || authentication, message = null) }
            try {
                if (!readOnly) cancelReads()
                session.repository.withOperation(readOnly) { session.block() }
            }
            catch (e: CancellationException) { throw e }
            catch (e: Exception) { failure(session, e) }
            finally {
                effects.remove(currentCoroutineContext()[Job]); readJobs.remove(currentCoroutineContext()[Job])
                session.publish { it.copy(managementRefreshing = false,
                    switchingLocked = if (authentication) false else it.switchingLocked) }
                mutable.update {
                    if (it.activeProfileId == session.context.profileId) it.copy(busy = effects.values.any { id -> id == session.context.profileId },
                        switchingLocked = if (authentication) false else it.switchingLocked)
                    else if (authentication) it.copy(switchingLocked = false) else it
                }
                processNotification()
            }
        }
        if (readOnly) readJobs.add(job) else effects[job] = session.context.profileId
        job.start()
    }
    private suspend fun Session.recorded(action: JournalAction, requestId: String? = null, block: suspend () -> Unit) {
        repository.journal.record(JournalEvent(action, JournalOutcome.STARTED, context = context, requestId = requestId))
        try { block(); repository.journal.record(JournalEvent(action, JournalOutcome.SUCCESS, context = context, requestId = requestId)) }
        catch (e: Exception) {
            repository.journal.record(JournalEvent(action, operationFailureOutcome(action, e, repository.managementPending && !repository.managementKnown),
                context = context, requestId = requestId, error = if (e is ApiException) JournalError.fromCode(e.code) else JournalError.OTHER))
            throw e
        }
    }
    fun pair(activity: Activity, code: String, name: String) = runOperation(authentication = true) {
        recorded(JournalAction.PAIR) { repository.pair(activity, code.trim().uppercase(), name.trim(), context.origin) }
        publish { it.copy(paired = true, deviceName = repository.displayName) }
        loadTargets(); loadManagement()
    }
    fun refresh(userInitiated: Boolean = true) = runOperation(readOnly = true) {
        loadTargets()
        if (context.targetId != null && state.value.selectedTargetId == context.targetId) loadTarget(userInitiated)
        loadManagement()
    }
    fun refreshManagement(userInitiated: Boolean = true) = runOperation(readOnly = true) {
        publish { it.copy(managementRefreshing = true) }; loadTargets(); loadManagement()
    }
    private suspend fun Session.loadTargets() {
        try {
            val list = repository.targets(context.origin)
            managementEnabled = list.managementEnabled
            cachedTargets[context.profileId] = list.targets
            var selected = repository.profile.selectedTargetId
            if (selected != null && list.targets.none { it.targetId == selected }) {
                profiles.selectTarget(context.profileId, null); selected = null
                publish { it.copy(message = "原电脑已从服务器清单移除，请重新选择电脑。", target = null, wake = null, waiting = false) }
            } else if (selected == null && !repository.targetSelectionInitialized && list.targets.isNotEmpty()) {
                selected = list.targets.first().targetId
                profiles.selectTarget(context.profileId, selected)
            }
            repository.markTargetSelectionInitialized()
            publish { it.copy(targets = list.targets, targetsCached = false, targetsAvailable = true,
                managementEnabled = list.managementEnabled, selectedTargetId = selected, server = "可达") }
            if (selected != null && context.targetId == null) {
                val target = repository.target(selected, requestOrigin = context.origin)
                val wake = repository.rememberedWake(selected)
                publish { it.copy(target = target, wake = wake, resultText = wake?.let(::resultLabel) ?: "尚无唤醒请求") }
            }
        } catch (e: CancellationException) { throw e }
        catch (e: Exception) {
            publish { it.copy(targets = cachedTargets[context.profileId].orEmpty(), targetsCached = cachedTargets[context.profileId] != null,
                targetsAvailable = false, target = null) }
            throw e
        }
    }
    private suspend fun Session.loadTarget(fresh: Boolean = false) {
        val id = context.targetId ?: return
        val target = repository.target(id, fresh, context.origin)
        publish { it.copy(target = target, server = "可达") }
        repository.journal.record(JournalEvent(JournalAction.TARGET_CHECK, JournalOutcome.SUCCESS,
            trigger = if (fresh) JournalTrigger.USER else JournalTrigger.AUTOMATIC, context = context,
            reachability = when (target.reachability) { "reachable" -> JournalReachability.REACHABLE; "unreachable" -> JournalReachability.UNREACHABLE; else -> JournalReachability.UNKNOWN },
            router = if (target.router == "available") JournalRouter.AVAILABLE else JournalRouter.UNAVAILABLE))
    }
    private suspend fun Session.loadManagement(replace: Boolean = false) {
        if (managementEnabled == false) {
            publishManagement(replace)
            return
        }
        try { recorded(JournalAction.AUTHORIZATION_STATUS) { repository.managementStatus(context.origin) } }
        finally { publishManagement(replace) }
    }
    private suspend fun Session.publishManagement(replace: Boolean = false) {
        withContext(NonCancellable) {
            reminders.update(context.profileId, repository.management, repository.managementKnown, repository.managementPending, replace)
        }
        val marker = if (repository.managementPending && !repository.managementKnown) "授权未知" else if (repository.management != null) "管理授权开放" else ""
        mutable.update { it.copy(authorizationMarkers = it.authorizationMarkers + (context.profileId to marker)) }
        publish { it.copy(management = repository.management, managementKnown = repository.managementKnown, managementPending = repository.managementPending) }
    }
    fun requestWake() {
        if (!state.value.targetsAvailable || state.value.selectedTargetId == null) return
        val prior = state.value.wake
        if (prior?.result in setOf("pending", "unknown")) mutable.update { it.copy(confirmNewWake = true) } else sendNewWake()
    }
    fun sendNewWake() {
        if (!state.value.targetsAvailable) return
        runOperation {
            val id = context.targetId ?: return@runOperation
            publish { it.copy(confirmNewWake = false) }
            dispatch(repository.newWake(id))
        }
    }
    fun retryOriginal() {
        val record = state.value.wake ?: return
        if (!state.value.targetsAvailable || record.gatewayConfirmed || record.createdAt.isBefore(Instant.now().minusSeconds(86400))) return
        runOperation { dispatch(record) }
    }
    fun queryOriginal() {
        val record = state.value.wake ?: return
        runOperation(readOnly = true) {
            val result = repository.queryWake(record, context.origin)
            withContext(NonCancellable) { repository.rememberWake(result); recordWake(result) }
            publish { it.copy(wake = result, resultText = resultLabel(result)) }
        }
    }
    private suspend fun Session.dispatch(record: WakeRecord) {
        val pending = record.copy(result = "pending")
        withContext(NonCancellable) {
            repository.rememberWake(pending)
            repository.journal.record(JournalEvent(JournalAction.WAKE_SUBMIT, JournalOutcome.STARTED, context = context.copy(targetId = record.targetId),
                requestId = record.requestId, wakeStage = WakeStage.SUBMITTED))
        }
        publish { it.copy(wake = pending, resultText = "请求中…") }
        var result = pending.copy(result = "unknown", gatewayConfirmed = false)
        try { result = repository.sendWake(pending, context.origin) }
        finally {
            withContext(NonCancellable) { repository.rememberWake(result); recordWake(result) }
            publish { it.copy(wake = result, resultText = resultLabel(result), waiting = result.result == "sent" || result.result == "pending") }
        }
        if (result.result == "sent" || result.result == "pending") beginPolling(this, result)
    }
    private fun Session.recordWake(record: WakeRecord) {
        val stage = when (record.result) { "sent" -> WakeStage.SENT; "failed" -> WakeStage.FAILED; "pending" -> WakeStage.PENDING; else -> WakeStage.UNKNOWN }
        val outcome = when (record.result) { "sent" -> JournalOutcome.SUCCESS; "failed" -> JournalOutcome.FAILURE; "pending" -> JournalOutcome.PENDING; else -> JournalOutcome.UNKNOWN }
        repository.journal.record(JournalEvent(JournalAction.WAKE_RESULT, outcome, context = context.copy(targetId = record.targetId),
            requestId = record.requestId, wakeStage = stage, error = JournalError.fromCode(record.code)))
    }
    private fun beginPolling(session: Session, record: WakeRecord) {
        if (!foreground || state.value.selectionGeneration != session.context.selectionGeneration) return
        poll?.cancel()
        poll = viewModelScope.launch {
            try {
                session.repository.withOperation(readOnly = true) {
                    val deadline = (record.sentAt ?: record.createdAt).plusSeconds(90)
                    while (foreground && Instant.now().isBefore(deadline)) {
                        delay(3000)
                        val target = session.repository.target(record.targetId, requestOrigin = session.context.origin)
                        session.publish { it.copy(target = target) }
                        if (target.reachability == "reachable" && target.checkedAt?.isAfter(record.sentAt ?: record.createdAt) == true) {
                            session.repository.journal.record(JournalEvent(JournalAction.WAKE_ONLINE, JournalOutcome.SUCCESS,
                                trigger = JournalTrigger.AUTOMATIC, context = session.context.copy(targetId = record.targetId),
                                requestId = record.requestId, wakeStage = WakeStage.CONFIRMED_ONLINE, reachability = JournalReachability.REACHABLE))
                            session.publish { it.copy(waiting = false, resultText = "已确认在线（发送后的新探测）") }; break
                        }
                    }
                }
            } catch (_: CancellationException) { }
            catch (e: Exception) { failure(session, e) }
            finally {
                readJobs.remove(currentCoroutineContext()[Job])
                session.publish { it.copy(waiting = false) }
            }
        }.also { readJobs.add(it) }
    }
    fun authorizeManagement(activity: Activity) = runOperation(authentication = true) {
        try { recorded(JournalAction.AUTHORIZATION_START) { repository.authorizeManagement(activity, context.origin) } }
        finally { publishManagement(true) }
        if (repository.autoOpenBrowser) launchBrowser(activity)
    }
    fun openManagement(activity: Activity) = runOperation { launchBrowser(activity) }
    private suspend fun Session.launchBrowser(activity: Activity) {
        val url = repository.managementBrowserUrl(context.origin)
        publishManagement()
        recorded(JournalAction.AUTHORIZATION_BROWSER) {
            activity.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)).addCategory(Intent.CATEGORY_BROWSABLE))
        }
    }
    fun endManagement() = runOperation {
        try { recorded(JournalAction.AUTHORIZATION_END) { repository.endManagement(context.origin) } }
        finally { publishManagement() }
    }
    private fun preference(block: suspend Repository.() -> Unit) = runOperation {
        recorded(JournalAction.SETTINGS_CHANGE) { repository.block() }
        publish { it.copy(sessionMinutes = repository.sessionMinutes, autoOpenBrowser = repository.autoOpenBrowser,
            warnAllDevices = repository.warnAllDevices, allowAllDevices = repository.allowAllDevices) }
        publishManagement()
    }
    fun selectSessionMinutes(value: Int) = preference { selectSessionMinutes(value) }
    fun setAutoOpenBrowser(value: Boolean) = preference { setAutoOpenBrowser(value) }
    fun setWarnAllDevices(value: Boolean) = preference { setWarnAllDevices(value) }
    fun setAllowAllDevices(value: Boolean) = preference { setAllowAllDevices(value) }
    internal fun lockSwitching(value: Boolean) { mutable.update { it.copy(switchingLocked = value) }; if (!value) viewModelScope.launch { processNotification() } }
    internal fun identityRevoked(profileId: String) {
        viewModelScope.launch {
            reminders.clear(profileId)
            if (state.value.activeProfileId == profileId) activate(profileId)
        }
    }
    internal fun recordDiagnostic(context: OperationContext, report: DiagnosticReport) {
        val outcome = when { report.stages.any { it.outcome == DiagnosticOutcome.CANCELED } -> JournalOutcome.CANCELLED
            report.stages.any { it.outcome == DiagnosticOutcome.FAILURE } -> JournalOutcome.FAILURE
            report.stages.any { it.outcome == DiagnosticOutcome.UNKNOWN } -> JournalOutcome.UNKNOWN
            else -> JournalOutcome.SUCCESS }
        journal.record(JournalEvent(JournalAction.CONNECTION_CHECK, outcome, context = context, durationMs = report.elapsedMs))
    }
    private suspend fun failure(session: Session, error: Exception) {
        if (error is IdentityLost) {
            withContext(NonCancellable) {
                session.repository.forgetIdentity()
            }
            if (state.value.activeProfileId == session.context.profileId) activate(session.context.profileId)
            mutable.update { if (it.activeProfileId == session.context.profileId) it.copy(message = "此服务器的本机身份已失效，请重新配对。") else it }
            return
        }
        session.publish { it.copy(message = errorMessage(error), server = if (error is NetworkException) "不可达" else it.server) }
    }
}

internal fun operationFailureOutcome(action: JournalAction, error: Exception, managementUncertain: Boolean): JournalOutcome = when {
    error is AuthenticationCancelled -> JournalOutcome.CANCELLED
    error is ApiException && error.status in 400..499 && error.code in setOf(
        "invalid_request", "unauthorized", "forbidden", "management_disabled", "management_inactive",
        "challenge_consumed_or_expired", "rate_limited", "protocol_upgrade_required"
    ) -> JournalOutcome.FAILURE
    managementUncertain && (action == JournalAction.AUTHORIZATION_START || action == JournalAction.AUTHORIZATION_END) -> JournalOutcome.UNKNOWN
    error is CancellationException -> JournalOutcome.CANCELLED
    else -> JournalOutcome.FAILURE
}

internal fun errorMessage(error: Exception): String = when (error) {
    is AuthenticationCancelled -> "已取消系统认证，已有授权不受影响。"
    is AuthenticationUnavailable -> "系统认证不可用；请检查安全锁屏与生物识别设置，不会降低密钥安全要求。"
    is NetworkException -> when (error.kind) {
        NetworkFailureKind.TLS -> "HTTPS 证书验证失败，未降低连接安全要求。"
        NetworkFailureKind.DNS -> "当前线路域名解析失败，请检查网络或系统 VPN。"
        else -> "当前线路连接失败，操作结果可能未知；不会自动切线或重发。"
    }
    is ApiException -> when (error.code) {
        "protocol_upgrade_required" -> "服务器版本不兼容，请升级服务器。"
        "target_not_found" -> "目标不存在，请刷新电脑清单。"
        "request_target_mismatch" -> "请求与目标不匹配，未重新发包。"
        "management_disabled" -> "此服务器未启用路由器管理。"
        "agent_unavailable", "upstream_unavailable" -> "执行端不可达；缓存电脑清单不能用于新唤醒。"
        "non_json_response" -> "线路未返回 API 响应，请检查服务地址。"
        "redirect_refused" -> "已拒绝服务器重定向，请检查线路配置。"
        else -> "服务器未完成操作，请刷新状态后重试。"
    }
    is IllegalArgumentException -> "输入或服务器响应无效，请检查名称、HTTPS 地址与服务器实例。"
    else -> "本机保存或操作未完成，请重试。"
}

fun resultLabel(record: WakeRecord): String = when (record.result) {
    "sent" -> "唤醒包已发送"
    "failed" -> when (record.code) { "target_not_found" -> "目标不存在，未发包"; "request_target_mismatch" -> "请求与目标不匹配，未发包"; else -> "唤醒包发送失败" }
    "pending" -> "请求处理中"
    else -> "发送结果未知，请查询原请求；不会自动重发"
}
