[CmdletBinding()]
param(
    [string]$DeploymentConfig = '',
    [ValidateSet('https://wol.hubu.wiki', 'https://wol-direct.hubu.wiki')]
    [string]$DirectOrigin = 'https://wol.hubu.wiki',
    [ValidateSet('https://roc.hubu.wiki')]
    [string]$FallbackOrigin = 'https://roc.hubu.wiki'
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$Root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (-not $DeploymentConfig) { $DeploymentConfig = Join-Path $Root 'private\deployment.json' }
$Tools = Join-Path $env:LOCALAPPDATA 'RocWol\tools'
$Signing = Join-Path $env:LOCALAPPDATA 'RocWol\signing'
$Dist = Join-Path $Root 'dist'
$Download = Join-Path $PSScriptRoot 'download.mjs'
$Utf8 = New-Object Text.UTF8Encoding($false)

function Invoke-Native {
    param([string]$File, [string[]]$Arguments)
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native command failed: $([IO.Path]::GetFileName($File)) (exit $LASTEXITCODE)" }
}
function Protect-Directory {
    param([string]$Directory)
    [IO.Directory]::CreateDirectory($Directory) | Out-Null
    $user = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $system = New-Object Security.Principal.SecurityIdentifier('S-1-5-18')
    $acl = Get-Acl -LiteralPath $Directory
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.Access)) { [void]$acl.RemoveAccessRuleSpecific($rule) }
    foreach ($identity in @($user, $system)) {
        $rule = New-Object Security.AccessControl.FileSystemAccessRule($identity, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
        $acl.AddAccessRule($rule)
    }
    [IO.Directory]::SetAccessControl($Directory, $acl)
}
function Protect-File {
    param([string]$Filename)
    $user = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $system = New-Object Security.Principal.SecurityIdentifier('S-1-5-18')
    $acl = Get-Acl -LiteralPath $Filename
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($rule in @($acl.Access)) { [void]$acl.RemoveAccessRuleSpecific($rule) }
    foreach ($identity in @($user, $system)) { $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($identity, 'FullControl', 'Allow'))) }
    [IO.File]::SetAccessControl($Filename, $acl)
}
function Download-Official {
    param([string]$Url, [string]$Destination, [string]$Algorithm = 'metadata', [string]$Checksum = '')
    if ($Algorithm -ne 'metadata' -and (Test-Path -LiteralPath $Destination)) {
        if ((Get-FileHash -LiteralPath $Destination -Algorithm $Algorithm).Hash.ToLowerInvariant() -eq $Checksum.ToLowerInvariant()) { return }
    }
    Invoke-Native 'node' @($Download, $Url, $Destination, $Algorithm, $Checksum)
}
function Random-Password {
    $bytes = New-Object byte[] 32
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $generator.GetBytes($bytes); return [Convert]::ToBase64String($bytes) } finally { $generator.Dispose(); [Array]::Clear($bytes, 0, $bytes.Length) }
}
function Set-ProcessEnvironment {
    param([string]$Name, [string]$Value)
    [Environment]::SetEnvironmentVariable($Name, $Value, 'Process')
}

Get-Command node -ErrorAction Stop | Out-Null
Get-Command go -ErrorAction Stop | Out-Null
$goVersion = (& go version) -join ' '
if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch '\bgo1\.26\.3\b') { throw 'Go 1.26.3 is required; do not silently change the approved toolchain.' }
[IO.Directory]::CreateDirectory($Tools) | Out-Null
[IO.Directory]::CreateDirectory($Dist) | Out-Null
$deployment = $null
if (Test-Path -LiteralPath $DeploymentConfig) {
    $deployment = Get-Content -LiteralPath $DeploymentConfig -Raw | ConvertFrom-Json
    $DirectOrigin = "https://$($deployment.directHost)"
    $FallbackOrigin = "https://$($deployment.fallbackHost)"
}
if ($DirectOrigin -notin @('https://wol.hubu.wiki', 'https://wol-direct.hubu.wiki') -or $FallbackOrigin -ne 'https://roc.hubu.wiki') { throw 'Deployment origins are outside the approved whitelist.' }

# Reuse the locally installed project JDK, not global java8path or a changed system JAVA_HOME.
$JavaHome = $null
foreach ($candidate in @(Get-ChildItem -LiteralPath $Tools -Directory -Filter 'jdk-17*' | Sort-Object Name -Descending)) {
    if (Test-Path -LiteralPath (Join-Path $candidate.FullName 'bin\javac.exe')) { $JavaHome = $candidate.FullName; break }
}
if (-not $JavaHome) {
    $jdkMetadata = Join-Path $Tools 'adoptium-17.json'
    Download-Official 'https://api.adoptium.net/v3/assets/latest/17/hotspot?architecture=x64&heap_size=normal&image_type=jdk&jvm_impl=hotspot&os=windows&vendor=eclipse' $jdkMetadata
    $assets = @(Get-Content -LiteralPath $jdkMetadata -Raw | ConvertFrom-Json)
    if ($assets.Count -eq 0) { throw 'Official Adoptium JDK17 metadata returned no Windows x64 package.' }
    $package = $assets[0].binary.package
    if ($package.checksum -notmatch '^[a-fA-F0-9]{64}$') { throw 'Official JDK SHA256 is missing.' }
    $jdkZip = Join-Path $Tools 'temurin17.zip'
    Download-Official $package.link $jdkZip 'sha256' $package.checksum
    Expand-Archive -LiteralPath $jdkZip -DestinationPath $Tools -Force
    $candidates = @(Get-ChildItem -LiteralPath $Tools -Directory -Filter 'jdk-17*' | Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName 'bin\javac.exe') } | Sort-Object Name -Descending)
    if ($candidates.Count -eq 0) { throw 'Verified JDK archive did not contain a JDK17 installation.' }
    $JavaHome = $candidates[0].FullName
}
$java = Join-Path $JavaHome 'bin\java.exe'
$javacOutput = (& (Join-Path $JavaHome 'bin\javac.exe') -version 2>&1) -join ' '
if ($LASTEXITCODE -ne 0 -or $javacOutput -notmatch '\bjavac 17\.') { throw 'Project JDK is not version 17.' }

$Sdk = Join-Path $Tools 'android-sdk'
$SdkManager = Join-Path $Sdk 'cmdline-tools\latest\bin\sdkmanager.bat'
if (-not (Test-Path -LiteralPath $SdkManager)) {
    $repository = Join-Path $Tools 'repository2-1.xml'
    Download-Official 'https://dl.google.com/android/repository/repository2-1.xml' $repository
    [xml]$sdkMetadata = Get-Content -LiteralPath $repository -Raw
    $package = $sdkMetadata.SelectSingleNode("//*[local-name()='remotePackage' and @path='cmdline-tools;latest']")
    if (-not $package) { throw 'Official Google metadata lacks cmdline-tools;latest.' }
    $channel = $package.SelectSingleNode("*[local-name()='channelRef']")
    if ($channel -and $channel.GetAttribute('ref') -ne 'channel-0') { throw 'Command-line tools metadata is not the stable channel.' }
    $archive = $package.SelectSingleNode("*[local-name()='archives']/*[local-name()='archive'][*[local-name()='host-os']='windows']/*[local-name()='complete']")
    if (-not $archive) { throw 'Official Google metadata lacks Windows command-line tools.' }
    $relativeUrl = $archive.SelectSingleNode("*[local-name()='url']").InnerText
    $checksumNode = $archive.SelectSingleNode("*[local-name()='checksum']")
    $algorithm = $checksumNode.GetAttribute('type').ToLowerInvariant()
    if (-not $algorithm) { $algorithm = 'sha1' }
    if ($algorithm -notin @('sha1', 'sha256')) { throw 'Unsupported official SDK checksum algorithm.' }
    if ($relativeUrl -notmatch '^commandlinetools-win-[0-9]+_latest\.zip$') { throw 'Unexpected official SDK archive path.' }
    $sdkZip = Join-Path $Tools $relativeUrl
    Download-Official "https://dl.google.com/android/repository/$relativeUrl" $sdkZip $algorithm $checksumNode.InnerText.Trim()
    $unpack = Join-Path $Tools ('sdk-unpack-' + [Guid]::NewGuid().ToString('N'))
    Expand-Archive -LiteralPath $sdkZip -DestinationPath $unpack
    [IO.Directory]::CreateDirectory((Join-Path $Sdk 'cmdline-tools')) | Out-Null
    Move-Item -LiteralPath (Join-Path $unpack 'cmdline-tools') -Destination (Join-Path $Sdk 'cmdline-tools\latest')
    Remove-Item -LiteralPath $unpack -Recurse -Force
}

$GradleHome = Join-Path $Tools 'gradle-8.13'
$gradleChecksumFile = Join-Path $Tools 'gradle-8.13-bin.zip.sha256'
Download-Official 'https://downloads.gradle.org/distributions/gradle-8.13-bin.zip.sha256' $gradleChecksumFile
$gradleChecksum = ([IO.File]::ReadAllText($gradleChecksumFile)).Trim().ToLowerInvariant()
if ($gradleChecksum -ne '20f1b1176237254a6fc204d8434196fa11a4cfb387567519c61556e8710aed78') { throw 'Gradle8.13 official checksum differs from approved pinned checksum.' }
$gradleZip = Join-Path $Tools 'gradle-8.13-bin.zip'
if (-not (Test-Path -LiteralPath (Join-Path $GradleHome 'bin\gradle.bat'))) {
    try { Download-Official 'https://services.gradle.org/distributions/gradle-8.13-bin.zip' $gradleZip 'sha256' $gradleChecksum }
    catch { Download-Official 'https://github.com/gradle/gradle-distributions/releases/download/v8.13.0/gradle-8.13-bin.zip' $gradleZip 'sha256' $gradleChecksum }
    Expand-Archive -LiteralPath $gradleZip -DestinationPath $Tools -Force
} elseif (-not (Test-Path -LiteralPath $gradleZip) -or (Get-FileHash -LiteralPath $gradleZip -Algorithm SHA256).Hash.ToLowerInvariant() -ne $gradleChecksum) {
    # Wrapper seeding still requires a byte-verified official distribution, never a repacked local install.
    Download-Official 'https://github.com/gradle/gradle-distributions/releases/download/v8.13.0/gradle-8.13-bin.zip' $gradleZip 'sha256' $gradleChecksum
}
$wrapper = Join-Path $Root 'android\gradle\wrapper\gradle-wrapper.properties'
$wrapperText = [IO.File]::ReadAllText($wrapper)
$distributionMatch = [regex]::Match($wrapperText, '(?m)^distributionUrl=(.+)\r?$')
$checksumMatch = [regex]::Match($wrapperText, '(?m)^distributionSha256Sum=([a-fA-F0-9]{64})\r?$')
if (-not $distributionMatch.Success -or -not $checksumMatch.Success -or $checksumMatch.Groups[1].Value.ToLowerInvariant() -ne $gradleChecksum) { throw 'Android wrapper must pin the official Gradle8.13 distribution SHA256.' }
$distributionUrl = $distributionMatch.Groups[1].Value.Trim().Replace('\:', ':')
if ($distributionUrl -notin @('https://services.gradle.org/distributions/gradle-8.13-bin.zip', 'https://downloads.gradle.org/distributions/gradle-8.13-bin.zip')) { throw 'Android wrapper distribution URL differs from the approved official Gradle8.13 URL.' }

Protect-Directory $Signing
$keystore = if ($env:ROCWOL_KEYSTORE) { [IO.Path]::GetFullPath($env:ROCWOL_KEYSTORE) } else { Join-Path $Signing 'roc-wol.p12' }
$storePasswordFile = if ($env:ROCWOL_STORE_PASSWORD_FILE) { [IO.Path]::GetFullPath($env:ROCWOL_STORE_PASSWORD_FILE) } else { Join-Path $Signing 'store-password.txt' }
$keyPasswordFile = if ($env:ROCWOL_KEY_PASSWORD_FILE) { [IO.Path]::GetFullPath($env:ROCWOL_KEY_PASSWORD_FILE) } else { Join-Path $Signing 'key-password.txt' }
foreach ($file in @($keystore, $storePasswordFile, $keyPasswordFile)) {
    $parentDirectory = [IO.Path]::GetDirectoryName($file)
    if (-not (Test-Path -LiteralPath $parentDirectory)) { Protect-Directory $parentDirectory }
}
if (($keystore -ne (Join-Path $Signing 'roc-wol.p12')) -and -not (Test-Path -LiteralPath $keystore)) { throw 'Custom keystore override must already exist; newly generated release identities use the protected default signing directory.' }
if (-not (Test-Path -LiteralPath $keystore)) {
    if ((Test-Path -LiteralPath $storePasswordFile) -or (Test-Path -LiteralPath $keyPasswordFile)) { throw 'Signing state is incomplete. Restore the existing protected keystore; do not silently replace its identity.' }
    $password = Random-Password
    [IO.File]::WriteAllText($storePasswordFile, $password, $Utf8)
    [IO.File]::WriteAllText($keyPasswordFile, $password, $Utf8)
    Protect-File $storePasswordFile
    Protect-File $keyPasswordFile
    try {
        Set-ProcessEnvironment 'ROCWOL_TEMP_STORE_PASSWORD' $password
        Invoke-Native (Join-Path $JavaHome 'bin\keytool.exe') @('-genkeypair', '-storetype', 'PKCS12', '-keystore', $keystore, '-alias', 'roc-wol', '-keyalg', 'RSA', '-keysize', '3072', '-validity', '10000', '-dname', 'CN=Roc WOL Release', '-storepass:env', 'ROCWOL_TEMP_STORE_PASSWORD', '-keypass:env', 'ROCWOL_TEMP_STORE_PASSWORD', '-noprompt')
    } finally { Set-ProcessEnvironment 'ROCWOL_TEMP_STORE_PASSWORD' $null; $password = $null }
}
foreach ($file in @($keystore, $storePasswordFile, $keyPasswordFile)) {
    if (-not (Test-Path -LiteralPath $file)) { throw 'Protected release signing files are incomplete.' }
    Protect-File $file
}

$envNames = @('JAVA_HOME', 'ANDROID_HOME', 'ANDROID_SDK_ROOT', 'GRADLE_USER_HOME', 'PATH', 'CGO_ENABLED', 'GOOS', 'GOARCH', 'GOTOOLCHAIN', 'ROCWOL_KEYSTORE', 'ROCWOL_STORE_PASSWORD_FILE', 'ROCWOL_KEY_PASSWORD_FILE')
$savedEnv = @{}
foreach ($name in $envNames) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
$location = Get-Location
try {
    Set-ProcessEnvironment 'JAVA_HOME' $JavaHome
    Set-ProcessEnvironment 'ANDROID_HOME' $Sdk
    Set-ProcessEnvironment 'ANDROID_SDK_ROOT' $Sdk
    $gradleUserHome = if ($savedEnv['GRADLE_USER_HOME']) { $savedEnv['GRADLE_USER_HOME'] } else { Join-Path $Root 'private\gradle-cache' }
    [IO.Directory]::CreateDirectory($gradleUserHome) | Out-Null
    Set-ProcessEnvironment 'GRADLE_USER_HOME' $gradleUserHome
    Set-ProcessEnvironment 'PATH' ((Join-Path $JavaHome 'bin') + ';' + $env:PATH)
    Set-ProcessEnvironment 'ROCWOL_KEYSTORE' $keystore
    Set-ProcessEnvironment 'ROCWOL_STORE_PASSWORD_FILE' $storePasswordFile
    Set-ProcessEnvironment 'ROCWOL_KEY_PASSWORD_FILE' $keyPasswordFile
    Set-ProcessEnvironment 'GOTOOLCHAIN' 'local'
    # Seed exactly the official wrapper URL's standard cache key using the verified ZIP.
    # Java BigInteger reproduces Gradle PathAssembler's MD5(base36) algorithm without PowerShell overflow.
    $seedJava = Join-Path $Tools 'RocWolWrapperSeed.java'
    $seedSource = @'
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.security.MessageDigest;
import java.util.zip.*;
public class RocWolWrapperSeed {
  public static void main(String[] a) throws Exception {
    String key = new BigInteger(1, MessageDigest.getInstance("MD5").digest(a[0].getBytes(StandardCharsets.UTF_8))).toString(36);
    Path dir = Path.of(a[1], "wrapper", "dists", "gradle-8.13-bin", key);
    Files.createDirectories(dir);
    Path zip = dir.resolve("gradle-8.13-bin.zip");
    Path marker = dir.resolve("gradle-8.13-bin.zip.ok");
    if (Files.isRegularFile(marker) && Files.isRegularFile(dir.resolve("gradle-8.13/bin/gradle.bat"))) return;
    Files.copy(Path.of(a[2]), zip, StandardCopyOption.REPLACE_EXISTING);
    try (ZipInputStream input = new ZipInputStream(Files.newInputStream(zip))) {
      for (ZipEntry entry; (entry = input.getNextEntry()) != null;) {
        Path target = dir.resolve(entry.getName()).normalize();
        if (!target.startsWith(dir)) throw new IllegalArgumentException("Unsafe official archive path");
        if (entry.isDirectory()) Files.createDirectories(target);
        else { Files.createDirectories(target.getParent()); Files.copy(input, target, StandardCopyOption.REPLACE_EXISTING); }
      }
    }
    Files.writeString(marker, "");
  }
}
'@
    [IO.File]::WriteAllText($seedJava, $seedSource, $Utf8)
    try { Invoke-Native $java @($seedJava, $distributionUrl, $gradleUserHome, $gradleZip) }
    finally { Remove-Item -LiteralPath $seedJava -Force -ErrorAction SilentlyContinue }

    $requiredPackages = @()
    if (-not (Test-Path -LiteralPath (Join-Path $Sdk 'platform-tools\adb.exe'))) { $requiredPackages += 'platform-tools' }
    if (-not (Test-Path -LiteralPath (Join-Path $Sdk 'platforms\android-36\android.jar'))) { $requiredPackages += 'platforms;android-36' }
    if (-not (Test-Path -LiteralPath (Join-Path $Sdk 'build-tools\35.0.0\apksigner.bat'))) { $requiredPackages += 'build-tools;35.0.0' }
    if ($requiredPackages.Count -gt 0) {
        Write-Host 'Review and accept the official Android SDK licenses when prompted.'
        $nativeSdkWrapper = Test-Path -LiteralPath (Join-Path ([IO.Path]::GetDirectoryName($SdkManager)) 'android.exe')
        if ($nativeSdkWrapper) {
            & $SdkManager "--sdk_root=$Sdk" '--licenses'
            if ($LASTEXITCODE -notin @(0, 9)) { throw "Android SDK license operation failed (exit $LASTEXITCODE)." }
            # Current native Android CLI wrapper uses slash package paths and may return 9 after installing.
            $nativePackages = @($requiredPackages | ForEach-Object { $_.Replace(';', '/') })
            & $SdkManager "--sdk_root=$Sdk" @nativePackages
            if ($LASTEXITCODE -notin @(0, 9)) { throw "Android SDK package installation failed (exit $LASTEXITCODE)." }
        } else {
            Invoke-Native $SdkManager @("--sdk_root=$Sdk", '--licenses')
            Invoke-Native $SdkManager (@("--sdk_root=$Sdk") + $requiredPackages)
        }
        foreach ($requiredFile in @('platform-tools\adb.exe', 'platforms\android-36\android.jar', 'build-tools\35.0.0\apksigner.bat')) {
            if (-not (Test-Path -LiteralPath (Join-Path $Sdk $requiredFile))) { throw "Official SDK installation is incomplete: $requiredFile" }
        }
    }
    Set-Location -LiteralPath $Root
    Set-ProcessEnvironment 'CGO_ENABLED' '0'
    Set-ProcessEnvironment 'GOOS' 'windows'
    Set-ProcessEnvironment 'GOARCH' 'amd64'
    $smokeDir = Join-Path $Tools ('go-smoke-' + [Guid]::NewGuid().ToString('N'))
    [IO.Directory]::CreateDirectory($smokeDir) | Out-Null
    try {
        Invoke-Native 'go' @('build', '-trimpath', '-o', (Join-Path $smokeDir 'roc-wol.exe'), './cmd/roc-wol')
        Invoke-Native 'go' @('build', '-trimpath', '-o', (Join-Path $smokeDir 'roc-wol-agent.exe'), './cmd/roc-wol-agent')
        Invoke-Native (Join-Path $smokeDir 'roc-wol.exe') @('--help')
        Invoke-Native (Join-Path $smokeDir 'roc-wol-agent.exe') @('--help')
    } finally { Remove-Item -LiteralPath $smokeDir -Recurse -Force -ErrorAction SilentlyContinue }
    Set-ProcessEnvironment 'GOOS' 'linux'
    Set-ProcessEnvironment 'GOARCH' 'amd64'
    Invoke-Native 'go' @('build', '-trimpath', '-ldflags=-s -w', '-o', (Join-Path $Dist 'roc-wol-linux-amd64'), './cmd/roc-wol')
    Set-ProcessEnvironment 'GOARCH' 'arm64'
    Invoke-Native 'go' @('build', '-trimpath', '-ldflags=-s -w', '-o', (Join-Path $Dist 'roc-wol-agent-linux-arm64'), './cmd/roc-wol-agent')
    Invoke-Native (Join-Path $Root 'android\gradlew.bat') @('-p', (Join-Path $Root 'android'), '--no-daemon', "-Procwol.directOrigin=$DirectOrigin", "-Procwol.fallbackOrigin=$FallbackOrigin", ':app:assembleRelease')
    $builtApk = Join-Path $Root 'android\app\build\outputs\apk\release\app-release.apk'
    if (-not (Test-Path -LiteralPath $builtApk)) { throw 'Signed release APK is missing; no debug APK substitution is allowed.' }
    $apk = Join-Path $Dist 'roc-wol-release.apk'
    Copy-Item -LiteralPath $builtApk -Destination $apk -Force
    $apksigner = Join-Path $Sdk 'build-tools\35.0.0\apksigner.bat'
    $verification = (& $apksigner verify --verbose --print-certs $apk 2>&1) -join [Environment]::NewLine
    if ($LASTEXITCODE -ne 0) { throw 'Release APK signature verification failed.' }
    Write-Host $verification
    $fingerprint = [regex]::Match($verification, 'Signer #1 certificate SHA-256 digest:\s*([a-fA-F0-9]+)').Groups[1].Value.ToLowerInvariant()
    if ($fingerprint -notmatch '^[a-f0-9]{64}$') { throw 'APK signer certificate fingerprint was not reported.' }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($apk)
    try {
        foreach ($entry in $zip.Entries) {
            if ($entry.FullName -match '(?i)(^|/)(SSH\.pem|账号密码\.txt|private|deployment\.json|agent\.json|gateway\.json)(/|$)|\.(?:p12|jks|key|pem)$') { throw 'Release APK contains a forbidden credential/configuration file.' }
            if ($entry.Length -gt 0) {
                $stream = $entry.Open()
                $reader = New-Object IO.StreamReader($stream, [Text.Encoding]::GetEncoding(28591))
                try {
                    $entryText = $reader.ReadToEnd()
                    if ($entryText -match '-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----') { throw 'Release APK contains private-key material.' }
                    if ($null -ne $deployment -and $deployment.PSObject.Properties.Name -contains 'stcpKey' -and $entryText.Contains([string]$deployment.stcpKey)) { throw 'Release APK contains the deployment STCP secret.' }
                } finally { $reader.Dispose(); $stream.Dispose() }
            }
        }
    } finally { $zip.Dispose() }
    $checksums = [ordered]@{}
    foreach ($name in @('roc-wol-linux-amd64', 'roc-wol-agent-linux-arm64', 'roc-wol-release.apk')) { $checksums[$name] = (Get-FileHash -LiteralPath (Join-Path $Dist $name) -Algorithm SHA256).Hash.ToLowerInvariant() }
    $manifest = [ordered]@{ version = 1; builtAt = [DateTime]::UtcNow.ToString('o'); directOrigin = $DirectOrigin; fallbackOrigin = $FallbackOrigin; apkCertificateSHA256 = $fingerprint; sha256 = $checksums }
    [IO.File]::WriteAllText((Join-Path $Dist 'build-manifest.json'), ($manifest | ConvertTo-Json -Depth 5), $Utf8)
    [IO.File]::WriteAllText((Join-Path $Dist 'roc-wol-release.apk.sha256'), "$($checksums['roc-wol-release.apk'])  roc-wol-release.apk`n", $Utf8)
    Write-Host "Signed release: $apk"
    Write-Host "APK certificate SHA256: $fingerprint"
    Write-Host "APK SHA256: $($checksums['roc-wol-release.apk'])"
} finally {
    Set-Location -LiteralPath $location.Path
    foreach ($name in $envNames) { Set-ProcessEnvironment $name $savedEnv[$name] }
}
