"use strict";
async function redeem() {
  const fragment = location.hash.slice(1);
  history.replaceState(null, "", location.pathname);
  const status = document.getElementById("status");
  const fail = () => { status.textContent = "授权已失效，请返回 App 重试"; };
  const params = new URLSearchParams(fragment);
  const ticket = params.get("t");
  if (!ticket || !/^[A-Za-z0-9_-]{43}$/.test(ticket) || params.size !== 1) { fail(); return; }
  try {
    const response = await fetch("/_wol/v1/manage/redeem", {
      method: "POST", credentials: "same-origin", cache: "no-store", redirect: "error",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ticket }), signal: AbortSignal.timeout(10000)
    });
    if (!response.ok) { fail(); return; }
    const result = await response.json();
    if (result.redirect !== "/cgi-bin/luci/") { fail(); return; }
    location.replace("/cgi-bin/luci/");
  } catch { fail(); }
}
window.addEventListener("hashchange", redeem);
void redeem();
