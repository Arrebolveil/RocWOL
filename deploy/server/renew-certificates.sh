#!/bin/sh
set -eu
umask 077
. /etc/roc-wol/renew.env
: "${DOMAINS:?}" "${WEBROOT_HOST:?}" "${TLS_DEST_HOST:?}" "${PROXY_MODE:?}"
case "$PROXY_MODE" in
    nginx) ;;
    onepanel) : "${OPENRESTY_CONTAINER:?}"; docker inspect "$OPENRESTY_CONTAINER" >/dev/null ;;
    *) exit 1 ;;
esac
for domain in $DOMAINS; do
    case "$domain" in *[!a-z0-9.-]*|'') exit 1 ;; esac
done
case "$WEBROOT_HOST:$TLS_DEST_HOST" in *'/../'*|*'/./'*) exit 1 ;; esac
case "$WEBROOT_HOST" in /*) ;; *) exit 1 ;; esac
case "$TLS_DEST_HOST" in /*) ;; *) exit 1 ;; esac
install_only=false
reload=true
for arg in "$@"; do
    case "$arg" in --install-only) install_only=true ;; --no-reload) reload=false ;; *) exit 1 ;; esac
done
exec 9>/run/roc-wol-renew.lock
flock -n 9 || exit 0
if ! $install_only; then
    certbot renew --cert-name roc-wol --webroot -w "$WEBROOT_HOST" --non-interactive --no-random-sleep-on-renew --config-dir /opt/roc-wol/acme/etc --work-dir /opt/roc-wol/acme/work --logs-dir /opt/roc-wol/acme/logs
fi
live=/opt/roc-wol/acme/etc/live/roc-wol
for file in cert.pem chain.pem fullchain.pem privkey.pem; do test -s "$live/$file"; done
openssl x509 -in "$live/cert.pem" -noout -checkend 604800 >/dev/null
for domain in $DOMAINS; do openssl x509 -in "$live/cert.pem" -noout -checkhost "$domain" >/dev/null; done
openssl verify -CApath /etc/ssl/certs -untrusted "$live/chain.pem" "$live/cert.pem" >/dev/null
cert_key=$(openssl x509 -in "$live/cert.pem" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum)
private_key=$(openssl pkey -in "$live/privkey.pem" -pubout -outform DER | sha256sum)
[ "$cert_key" = "$private_key" ]
mkdir -p "$TLS_DEST_HOST" /opt/roc-wol/rollback
backup=$(mktemp -d /opt/roc-wol/rollback/tls.XXXXXX)
changed=false
proxy() {
    if [ "$PROXY_MODE" = nginx ]; then nginx "$1" ${2:+"$2"};
    else docker exec "$OPENRESTY_CONTAINER" openresty "$1" ${2:+"$2"}; fi
}
rollback() {
    for file in fullchain.pem privkey.pem; do
        if [ -f "$backup/$file" ]; then
            cp -p "$backup/$file" "$TLS_DEST_HOST/$file.rollback"
            mv -f "$TLS_DEST_HOST/$file.rollback" "$TLS_DEST_HOST/$file"
        else rm -f "$TLS_DEST_HOST/$file"; fi
        rm -f "$TLS_DEST_HOST/$file.new"
    done
}
finish() {
    status=$?
    trap - EXIT HUP INT TERM
    if [ "$status" -ne 0 ] && $changed; then
        rollback
        if $reload; then proxy -t >/dev/null 2>&1 && proxy -s reload >/dev/null 2>&1 || true; fi
    fi
    exit "$status"
}
trap finish EXIT
trap 'exit 1' HUP INT TERM
for file in fullchain.pem privkey.pem; do
    test ! -L "$TLS_DEST_HOST/$file"
    if [ -e "$TLS_DEST_HOST/$file" ]; then
        cp -p "$TLS_DEST_HOST/$file" "$backup/$file"
        sha256sum "$backup/$file" > "$backup/$file.sha256"
    fi
    install -m 600 "$live/$file" "$TLS_DEST_HOST/$file.new"
done
changed=true
for file in fullchain.pem privkey.pem; do mv -f "$TLS_DEST_HOST/$file.new" "$TLS_DEST_HOST/$file"; done
if $reload; then proxy -t >/dev/null 2>&1; proxy -s reload >/dev/null 2>&1; fi
changed=false
openssl x509 -in "$live/cert.pem" -noout -enddate
