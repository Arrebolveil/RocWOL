package wiki.hubu.rocwol.data

import android.content.Context
import android.net.DnsResolver
import android.os.CancellationSignal
import android.os.SystemClock
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.CoroutineStart
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.async
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.Job
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import java.net.InetAddress
import java.net.URI
import java.time.Instant
import kotlin.coroutines.resume
import wiki.hubu.rocwol.security.IdentityLost

// Reports deliberately contain no hostnames, addresses, credentials, or exception messages.
enum class DiagnosticStage { DNS, HTTPS, API, ROUTER, TARGET }
enum class DiagnosticOutcome { NOT_RUN, RUNNING, SUCCESS, FAILURE, UNKNOWN, SKIPPED, CANCELED }
enum class DiagnosticDetail {
    WAITING, CHECKING, SYSTEM_DNS_RESOLVED, DNS_FAILED, DNS_UNAVAILABLE,
    HTTPS_RESPONSE_RECEIVED, TLS_FAILED, CONNECTION_FAILED, NETWORK_FAILED, TIMED_OUT,
    TARGET_QUERY_SUCCEEDED, API_REJECTED, RESPONSE_INVALID, AUTHORIZATION_REQUIRED,
    ROUTER_AVAILABLE, ROUTER_UNAVAILABLE, TARGET_REACHABLE, TARGET_UNREACHABLE, TARGET_UNKNOWN,
    NO_OBSERVATION, PREVIOUS_STAGE_FAILED, CANCELED, PREVIOUS_CHECK_FINISHING
}

data class DiagnosticStageResult(
    val stage: DiagnosticStage,
    val outcome: DiagnosticOutcome = DiagnosticOutcome.NOT_RUN,
    val detail: DiagnosticDetail = DiagnosticDetail.WAITING,
    val elapsedMs: Long? = null,
    val failureKind: NetworkFailureKind? = null,
    val observedAt: Instant? = null
)

data class DiagnosticReport(
    val routeIndex: Int,
    val startedAt: Instant,
    val stages: List<DiagnosticStageResult>,
    val elapsedMs: Long = 0,
    val finished: Boolean = false
) {
    fun canceled(elapsedMs: Long): DiagnosticReport = copy(
        elapsedMs = elapsedMs,
        finished = true,
        stages = stages.map {
            when (it.outcome) {
                DiagnosticOutcome.RUNNING -> it.copy(outcome = DiagnosticOutcome.CANCELED, detail = DiagnosticDetail.CANCELED)
                DiagnosticOutcome.NOT_RUN -> it.copy(outcome = DiagnosticOutcome.SKIPPED, detail = DiagnosticDetail.CANCELED)
                else -> it
            }
        }
    )

    companion object {
        fun pending(routeIndex: Int): DiagnosticReport {
            require(routeIndex > 0)
            return DiagnosticReport(
                routeIndex, Instant.now(), DiagnosticStage.entries.map { DiagnosticStageResult(it) }
            )
        }
    }
}

class ConnectionDiagnostics private constructor(context: Context) {
    private val callbackExecutor = context.applicationContext.mainExecutor
    private val workerScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val queryGate = Mutex()
    @Volatile private var activeWorker: Job? = null
    @Volatile private var activeRun: Job? = null

    suspend fun cancelAndJoin() {
        withContext(NonCancellable) {
            activeRun?.cancelAndJoin()
            activeWorker?.cancelAndJoin()
        }
    }

    /** One system DNS lookup, then one authenticated cached target query. Never sends WOL. */
    suspend fun run(
        origin: String,
        routeIndex: Int,
        loadTarget: suspend () -> Target,
        onProgress: (DiagnosticReport) -> Unit = {}
    ): DiagnosticReport {
        val host = requireNotNull(URI(Origins.checked(origin)).host)
        val started = SystemClock.elapsedRealtime()
        var report = DiagnosticReport.pending(routeIndex)
        fun publish() {
            report = report.copy(elapsedMs = SystemClock.elapsedRealtime() - started)
            onProgress(report)
        }
        fun stage(
            stage: DiagnosticStage, outcome: DiagnosticOutcome, detail: DiagnosticDetail,
            elapsedMs: Long? = null, kind: NetworkFailureKind? = null, observedAt: Instant? = null
        ) {
            report = report.copy(stages = report.stages.map {
                if (it.stage == stage) DiagnosticStageResult(stage, outcome, detail, elapsedMs, kind, observedAt) else it
            })
        }
        fun finish(): DiagnosticReport {
            report = report.copy(finished = true, stages = report.stages.map {
                if (it.outcome == DiagnosticOutcome.NOT_RUN) it.copy(
                    outcome = DiagnosticOutcome.SKIPPED, detail = DiagnosticDetail.PREVIOUS_STAGE_FAILED
                ) else it
            })
            publish()
            return report
        }
        if (!queryGate.tryLock()) {
            report = report.copy(stages = report.stages.map {
                it.copy(outcome = DiagnosticOutcome.SKIPPED, detail = DiagnosticDetail.PREVIOUS_CHECK_FINISHING)
            })
            return finish()
        }
        var workerOwnsGate = false
        val runJob = currentCoroutineContext()[Job]
        activeRun = runJob
        try {
            currentCoroutineContext().ensureActive()
            stage(DiagnosticStage.DNS, DiagnosticOutcome.RUNNING, DiagnosticDetail.CHECKING)
            publish()
            val dnsStarted = SystemClock.elapsedRealtime()
            val dns = try {
                withTimeout(5_000) { resolve(host) }
            } catch (_: TimeoutCancellationException) {
                DiagnosticDetail.TIMED_OUT
            }
            currentCoroutineContext().ensureActive()
            stage(DiagnosticStage.DNS,
                when (dns) {
                    DiagnosticDetail.SYSTEM_DNS_RESOLVED -> DiagnosticOutcome.SUCCESS
                    DiagnosticDetail.DNS_UNAVAILABLE -> DiagnosticOutcome.UNKNOWN
                    else -> DiagnosticOutcome.FAILURE
                },
                dns, SystemClock.elapsedRealtime() - dnsStarted,
                if (dns == DiagnosticDetail.TIMED_OUT) NetworkFailureKind.TIMEOUT else if (dns == DiagnosticDetail.DNS_FAILED) NetworkFailureKind.DNS else null)
            if (dns != DiagnosticDetail.SYSTEM_DNS_RESOLVED) return finish()

            stage(DiagnosticStage.HTTPS, DiagnosticOutcome.RUNNING, DiagnosticDetail.CHECKING)
            stage(DiagnosticStage.API, DiagnosticOutcome.RUNNING, DiagnosticDetail.CHECKING)
            publish()
            val apiStarted = SystemClock.elapsedRealtime()
            // Track the actual blocking HTTP worker, not just the screen job. The gate remains
            // held through socket cleanup, and cancelAndJoin waits for that cleanup before switching.
            val worker = workerScope.async(start = CoroutineStart.LAZY) { loadTarget() }
            activeWorker = worker
            worker.invokeOnCompletion {
                queryGate.unlock()
                if (activeWorker === worker) activeWorker = null
            }
            workerOwnsGate = true
            try {
                val target = withTimeout(20_000) { worker.await() }
                currentCoroutineContext().ensureActive()
                stage(DiagnosticStage.HTTPS, DiagnosticOutcome.SUCCESS, DiagnosticDetail.HTTPS_RESPONSE_RECEIVED)
                stage(DiagnosticStage.API, DiagnosticOutcome.SUCCESS, DiagnosticDetail.TARGET_QUERY_SUCCEEDED,
                    SystemClock.elapsedRealtime() - apiStarted)
                stage(DiagnosticStage.ROUTER,
                    if (target.router == "available") DiagnosticOutcome.SUCCESS else DiagnosticOutcome.FAILURE,
                    if (target.router == "available") DiagnosticDetail.ROUTER_AVAILABLE else DiagnosticDetail.ROUTER_UNAVAILABLE,
                    observedAt = target.checkedAt)
                stage(DiagnosticStage.TARGET,
                    when (target.reachability) {
                        "reachable" -> DiagnosticOutcome.SUCCESS
                        "unreachable" -> DiagnosticOutcome.FAILURE
                        else -> DiagnosticOutcome.UNKNOWN
                    }, when (target.reachability) {
                        "reachable" -> DiagnosticDetail.TARGET_REACHABLE
                        "unreachable" -> DiagnosticDetail.TARGET_UNREACHABLE
                        else -> DiagnosticDetail.TARGET_UNKNOWN
                    }, observedAt = target.checkedAt)
            } catch (error: Exception) {
                if (error is CancellationException && error !is TimeoutCancellationException) throw error
                currentCoroutineContext().ensureActive()
                val kind = when (error) {
                    is TimeoutCancellationException -> NetworkFailureKind.TIMEOUT
                    is NetworkException -> error.kind
                    else -> null
                }
                val detail = when {
                    error is IdentityLost -> DiagnosticDetail.AUTHORIZATION_REQUIRED
                    error is ApiException && (error.status == 401 || error.status == 403) -> DiagnosticDetail.AUTHORIZATION_REQUIRED
                    error is ApiException -> DiagnosticDetail.API_REJECTED
                    kind == NetworkFailureKind.DNS -> DiagnosticDetail.DNS_FAILED
                    kind == NetworkFailureKind.TLS -> DiagnosticDetail.TLS_FAILED
                    kind == NetworkFailureKind.TIMEOUT -> DiagnosticDetail.TIMED_OUT
                    kind == NetworkFailureKind.CONNECTION -> DiagnosticDetail.CONNECTION_FAILED
                    kind != null -> DiagnosticDetail.NETWORK_FAILED
                    else -> DiagnosticDetail.RESPONSE_INVALID
                }
                stage(DiagnosticStage.HTTPS,
                    when {
                        error is ApiException -> DiagnosticOutcome.SUCCESS
                        kind == NetworkFailureKind.TLS -> DiagnosticOutcome.FAILURE
                        else -> DiagnosticOutcome.UNKNOWN
                    }, when {
                        error is ApiException -> DiagnosticDetail.HTTPS_RESPONSE_RECEIVED
                        kind == NetworkFailureKind.TLS -> DiagnosticDetail.TLS_FAILED
                        else -> DiagnosticDetail.NO_OBSERVATION
                    },
                    kind = kind)
                stage(DiagnosticStage.API, DiagnosticOutcome.FAILURE, detail, SystemClock.elapsedRealtime() - apiStarted, kind)
            } finally {
                worker.cancel()
            }
            return finish()
        } finally {
            if (!workerOwnsGate) queryGate.unlock()
            if (activeRun === runJob) activeRun = null
        }
    }

    private suspend fun resolve(host: String): DiagnosticDetail = suspendCancellableCoroutine { continuation ->
        val cancellation = CancellationSignal()
        continuation.invokeOnCancellation { cancellation.cancel() }
        try {
            // A null Network uses Android's default resolver/network, including system VPN policy.
            DnsResolver.getInstance().query(null, host, DnsResolver.FLAG_EMPTY, callbackExecutor, cancellation,
                object : DnsResolver.Callback<List<InetAddress>> {
                    override fun onAnswer(answer: List<InetAddress>, rcode: Int) {
                        continuation.resume(if (rcode == 0 && answer.isNotEmpty()) DiagnosticDetail.SYSTEM_DNS_RESOLVED else DiagnosticDetail.DNS_FAILED)
                    }
                    override fun onError(error: DnsResolver.DnsException) {
                        continuation.resume(DiagnosticDetail.DNS_FAILED)
                    }
                })
        } catch (_: SecurityException) {
            continuation.resume(DiagnosticDetail.DNS_UNAVAILABLE)
        } catch (_: IllegalArgumentException) {
            continuation.resume(DiagnosticDetail.DNS_UNAVAILABLE)
        }
    }

    companion object {
        @Volatile private var instance: ConnectionDiagnostics? = null
        fun get(context: Context): ConnectionDiagnostics = instance ?: synchronized(this) {
            instance ?: ConnectionDiagnostics(context.applicationContext).also { instance = it }
        }
    }
}
