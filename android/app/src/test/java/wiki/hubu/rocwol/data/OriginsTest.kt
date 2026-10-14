package wiki.hubu.rocwol.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Test

class OriginsTest {
    @Test fun canonicalHttpsOrigins() {
        mapOf(
            "https://wol.example" to "https://wol.example",
            "HTTPS://WOL.EXAMPLE/" to "https://wol.example",
            "https://wol.example:443" to "https://wol.example",
            "https://wol.example:0443/" to "https://wol.example",
            "https://wol.example:8443" to "https://wol.example:8443",
            "https://192.0.2.10" to "https://192.0.2.10",
            "https://[2001:db8::1]:8443/" to "https://[2001:db8::1]:8443"
        ).forEach { (input, expected) -> assertEquals(expected, Origins.checked(input)) }
    }
    @Test fun unsafeOrAmbiguousOriginsAreRejected() {
        listOf("http://wol.example", "https://user@wol.example", "https://wol.example/a",
            "https://wol.example//", "https://wol.example?x=1", "https://wol.example#x",
            " https://wol.example", "https://wol.example\n", "https://wöl.example",
            "https://%77ol.example", "https://wol.example\\evil", "https://wol.example:",
            "https://wol.example:0", "https://wol.example:65536", "https://wol.example.",
            "https://-wol.example", "https://wol_.example", "https://2001:db8::1",
            "https://256.0.0.1", "https://192.00.2.1").forEach {
            assertFalse(it, runCatching { Origins.checked(it) }.isSuccess)
        }
    }
    @Test fun onlyIndividuallyVerifiedRoutesBecomeCredentialDestinations() {
        val id = "0123456789abcdef0123456789abcdef"
        val profile = ServerProfile("00000000-0000-0000-0000-000000000002", "服务器", id,
            listOf(ServerOrigin("https://one.example", 1, id), ServerOrigin("https://two.example", 2, null)),
            3, "https://one.example", null, "roc_wol_00000000-0000-0000-0000-000000000002_wake_v1",
            "roc_wol_00000000-0000-0000-0000-000000000002_manage_v1")
        assertEquals(listOf("https://one.example"), profile.verifiedOrigins)
        assertEquals(emptyList<String>(), profile.copy(instanceId = null).verifiedOrigins)
    }
    @Test fun targetIdsAreNotDeviceRequestIdsOrCommands() {
        assertEquals("pc-a", checkedTargetId("pc-a"))
        listOf("", "PC-A", "../pc-a", "pc a", "pc;reboot", "a".repeat(65)).forEach {
            assertFalse(it, runCatching { checkedTargetId(it) }.isSuccess)
        }
    }
}
