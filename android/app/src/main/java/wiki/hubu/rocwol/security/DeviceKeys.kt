package wiki.hubu.rocwol.security

import android.app.Activity
import android.app.KeyguardManager
import android.hardware.biometrics.BiometricManager
import android.hardware.biometrics.BiometricPrompt
import android.os.CancellationSignal
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.MessageDigest
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

class AuthenticationCancelled : Exception()
class AuthenticationUnavailable(cause: Throwable? = null) : Exception(cause)
class IdentityLost : Exception()

class DeviceKeys {
    companion object {
        const val WAKE = "roc_wol_wake_v1"
        const val MANAGE = "roc_wol_manage_v1"
        fun encode(bytes: ByteArray): String = Base64.encodeToString(bytes, Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)
        fun hash(bytes: ByteArray): String = MessageDigest.getInstance("SHA-256").digest(bytes).joinToString("") { "%02x".format(it.toInt() and 255) }
    }
    private fun store(): KeyStore = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
    fun hasIdentity(): Boolean = store().let { it.containsAlias(WAKE) && it.containsAlias(MANAGE) }
    fun removeLocalKeys() { store().apply { deleteEntry(WAKE); deleteEntry(MANAGE) } }
    suspend fun preparePairing(activity: Activity) = withContext(Dispatchers.IO) {
        val keyguard = activity.getSystemService(KeyguardManager::class.java)
        val biometric = activity.getSystemService(BiometricManager::class.java)
        if (!keyguard.isDeviceSecure || biometric.canAuthenticate(BiometricManager.Authenticators.BIOMETRIC_STRONG or BiometricManager.Authenticators.DEVICE_CREDENTIAL) != BiometricManager.BIOMETRIC_SUCCESS) {
            throw AuthenticationUnavailable()
        }
        // Only unpaired devices invoke this; never silently replace an established identity.
        if (!hasIdentity()) {
            removeLocalKeys()
            generate(WAKE, false)
            generate(MANAGE, true)
        }
    }
    private fun generate(alias: String, authenticated: Boolean) {
        val spec = KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setUserAuthenticationRequired(authenticated)
        if (authenticated) {
            spec.setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG or KeyProperties.AUTH_DEVICE_CREDENTIAL)
        }
        KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply { initialize(spec.build()) }.generateKeyPair()
    }
    suspend fun publicKey(alias: String): ByteArray = withContext(Dispatchers.IO) {
        store().getCertificate(alias)?.publicKey?.encoded ?: throw IdentityLost()
    }
    private fun signer(alias: String): Signature {
        val key = store().getKey(alias, null) as? PrivateKey ?: throw IdentityLost()
        return Signature.getInstance("SHA256withECDSA").apply { initSign(key) }
    }
    suspend fun signWake(message: String): String = withContext(Dispatchers.IO) {
        val signature = signer(WAKE)
        signature.update(message.toByteArray(Charsets.UTF_8))
        encode(signature.sign())
    }
    suspend fun signManagement(activity: Activity, message: String, sessionMinutes: Int?, allowAllDevices: Boolean): String {
        val signature = withContext(Dispatchers.IO) {
            try { signer(MANAGE) }
            catch (e: java.security.GeneralSecurityException) { throw AuthenticationUnavailable(e) }
        }
        val authenticated = withContext(Dispatchers.Main.immediate) {
            suspendCancellableCoroutine<Signature> { continuation ->
                val cancellation = CancellationSignal()
                continuation.invokeOnCancellation { cancellation.cancel() }
                try {
                    val prompt = BiometricPrompt.Builder(activity)
                        .setTitle(if (sessionMinutes == null) "授权此手机" else "授权路由器管理")
                        .setSubtitle(when {
                            sessionMinutes == null -> "验证管理密钥后才能完成双密钥配对"
                            allowAllDevices -> "允许所有设备访问当前线路，授权 $sessionMinutes 分钟"
                            else -> "仅授权本机打开的浏览器，授权 $sessionMinutes 分钟"
                        })
                        .setDescription(when {
                            sessionMinutes == null -> "此认证用于注册管理密钥，不会开启路由器管理授权。"
                            allowAllDevices -> "授权期间，任何设备均可通过当前线路访问 LuCI 登录页，仍需 LuCI 登录。返回 App 不会取消授权；到期自动失效，也可在 App 手动取消。"
                            else -> "授权从本次签名被服务器接受时开始，仍需 LuCI 登录。返回 App 不会取消授权；到期自动失效，也可在 App 手动取消。"
                        })
                        .setAllowedAuthenticators(BiometricManager.Authenticators.BIOMETRIC_STRONG or BiometricManager.Authenticators.DEVICE_CREDENTIAL)
                        .build()
                    prompt.authenticate(BiometricPrompt.CryptoObject(signature), cancellation, activity.mainExecutor,
                        object : BiometricPrompt.AuthenticationCallback() {
                            override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                                if (!continuation.isActive) return
                                val verified = result.cryptoObject?.signature
                                if (verified == null) continuation.resumeWithException(AuthenticationUnavailable())
                                else continuation.resume(verified)
                            }
                            override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                                if (!continuation.isActive) return
                                val cancelled = errorCode == BiometricPrompt.BIOMETRIC_ERROR_USER_CANCELED || errorCode == BiometricPrompt.BIOMETRIC_ERROR_CANCELED
                                continuation.resumeWithException(if (cancelled) AuthenticationCancelled() else AuthenticationUnavailable())
                            }
                        })
                } catch (_: IllegalArgumentException) {
                    if (continuation.isActive) continuation.resumeWithException(AuthenticationUnavailable())
                } catch (_: IllegalStateException) {
                    if (continuation.isActive) continuation.resumeWithException(AuthenticationUnavailable())
                }
            }
        }
        return withContext(Dispatchers.IO) {
            try {
                authenticated.update(message.toByteArray(Charsets.UTF_8))
                encode(authenticated.sign())
            } catch (e: java.security.GeneralSecurityException) {
                throw AuthenticationUnavailable(e)
            }
        }
    }
}
