import java.io.File
import java.net.URI

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("com.google.devtools.ksp")
}

val signingRoot = File(System.getenv("LOCALAPPDATA") ?: System.getProperty("user.home"), "RocWol/signing")
fun signingFile(property: String, variable: String, name: String): File =
    File(providers.gradleProperty(property).orNull ?: System.getenv(variable) ?: File(signingRoot, name).path)
val releaseStore = signingFile("rocwol.keystore", "ROCWOL_KEYSTORE", "roc-wol.p12")
val storePasswordFile = signingFile("rocwol.storePasswordFile", "ROCWOL_STORE_PASSWORD_FILE", "store-password.txt")
val keyPasswordFile = signingFile("rocwol.keyPasswordFile", "ROCWOL_KEY_PASSWORD_FILE", "key-password.txt")
val signingReady = releaseStore.isFile && storePasswordFile.isFile && keyPasswordFile.isFile

android {
    val releasesUrl = providers.gradleProperty("rocwol.releasesUrl")
        .orElse("https://github.com/Arrebolveil/RocWOL/releases").get()
    require(releasesUrl.isEmpty() || runCatching {
        val uri = URI(releasesUrl)
        uri.scheme == "https" && !uri.host.isNullOrEmpty() && uri.rawUserInfo == null &&
            releasesUrl.none { it.isISOControl() } && !releasesUrl.contains('\\')
    }.getOrDefault(false)) { "rocwol.releasesUrl must be an HTTPS URL or empty" }
    namespace = "wiki.hubu.rocwol"
    compileSdk = 36
    defaultConfig {
        applicationId = "wiki.hubu.rocwol"
        minSdk = 30
        targetSdk = 36
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
        versionCode = 8
        versionName = "2.0.0"
        buildConfigField("String", "RELEASES_URL", "\"${releasesUrl.replace("\"", "\\\"")}\"")
    }
    signingConfigs {
        create("release") {
            if (signingReady) {
                storeFile = releaseStore
                storeType = "PKCS12"
                storePassword = storePasswordFile.readText().trimEnd('\r', '\n')
                keyAlias = "roc-wol"
                keyPassword = keyPasswordFile.readText().trimEnd('\r', '\n')
                require(!storePassword.isNullOrEmpty() && !keyPassword.isNullOrEmpty()) { "Release signing password files must not be empty" }
            }
        }
    }
    buildTypes {
        release {
            isDebuggable = false
            isMinifyEnabled = false
            signingConfig = signingConfigs.getByName("release")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions { jvmTarget = "17" }
    buildFeatures { compose = true; buildConfig = true }
}

tasks.configureEach {
    if (name == "preReleaseBuild") {
        doFirst {
            check(signingReady) { "Protected RocWol release keystore and password files are required; unsigned/debug fallback is forbidden" }
        }
    }
}

dependencies {
    implementation(platform("androidx.compose:compose-bom:2025.12.01"))
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.foundation:foundation")
    implementation("androidx.compose.material:material-icons-core")
    implementation("androidx.activity:activity-compose:1.11.0")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.9.4")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.9.4")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")
    implementation("androidx.room:room-runtime:2.8.4")
    implementation("androidx.room:room-ktx:2.8.4")
    ksp("androidx.room:room-compiler:2.8.4")
    implementation("androidx.work:work-runtime-ktx:2.10.1")
    testImplementation("junit:junit:4.13.2")
    androidTestImplementation("androidx.test.ext:junit:1.3.0")
    androidTestImplementation("androidx.test:runner:1.7.0")
}

ksp {
    arg("room.schemaLocation", "$projectDir/schemas")
}
