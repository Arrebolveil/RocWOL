#!/bin/sh
set -eu
umask 077
# Generated exclusively from the protected deployment configuration.
. /etc/roc-wol/renew.env
case "$CERTBOT_IMAGE" in certbot/certbot@sha256:*) ;; *) exit 1 ;; esac
case "$DIRECT_HOST" in wol.hubu.wiki|wol-direct.hubu.wiki) ;; *) exit 1 ;; esac
[ "$FALLBACK_HOST" = roc.hubu.wiki ]
exec 9>/run/roc-wol-renew.lock
flock -n 9 || exit 0
if [ "${1:-}" != --install-only ]; then
    docker run --rm -v /opt/roc-wol/acme/etc:/etc/letsencrypt -v /opt/1panel/apps/openresty/openresty/root:/var/www/acme "$CERTBOT_IMAGE" renew --cert-name roc-wol --webroot -w /var/www/acme --non-interactive --no-random-sleep-on-renew
fi
live=/opt/roc-wol/acme/etc/live/roc-wol
for file in cert.pem chain.pem fullchain.pem privkey.pem; do test -s "$live/$file"; done
openssl x509 -in "$live/cert.pem" -noout -checkend 604800 >/dev/null
openssl x509 -in "$live/cert.pem" -noout -checkhost "$DIRECT_HOST" >/dev/null
openssl x509 -in "$live/cert.pem" -noout -checkhost "$FALLBACK_HOST" >/dev/null
openssl verify -CApath /etc/ssl/certs -untrusted "$live/chain.pem" "$live/cert.pem" >/dev/null
cert_key=$(openssl x509 -in "$live/cert.pem" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum)
private_key=$(openssl pkey -in "$live/privkey.pem" -pubout -outform DER | sha256sum)
[ "$cert_key" = "$private_key" ]
backup=$(mktemp -d /root/roc-wol-private/tls-rollback.XXXXXX)
rollback() {
    for host in "$DIRECT_HOST" "$FALLBACK_HOST"; do
        dest=/opt/1panel/www/sites/$host/ssl
        for file in fullchain.pem privkey.pem; do
            if [ -f "$backup/$host.$file" ]; then
                cp "$backup/$host.$file" "$dest/$file.rollback"
                chmod 600 "$dest/$file.rollback"
                mv -f "$dest/$file.rollback" "$dest/$file"
            elif [ -f "$backup/$host.$file.absent" ]; then
                rm -f "$dest/$file"
            fi
        done
    done
}
for host in "$DIRECT_HOST" "$FALLBACK_HOST"; do
    dest=/opt/1panel/www/sites/$host/ssl
    mkdir -p "$dest"
    for file in fullchain.pem privkey.pem; do
        if [ -e "$dest/$file" ]; then cp -p "$dest/$file" "$backup/$host.$file"; else touch "$backup/$host.$file.absent"; fi
        install -m 600 "$live/$file" "$dest/$file.new"
    done
done
for host in "$DIRECT_HOST" "$FALLBACK_HOST"; do
    dest=/opt/1panel/www/sites/$host/ssl
    mv -f "$dest/fullchain.pem.new" "$dest/fullchain.pem"
    mv -f "$dest/privkey.pem.new" "$dest/privkey.pem"
done
if ! docker exec "$OPENRESTY_CONTAINER" openresty -t >/dev/null 2>&1; then rollback; exit 1; fi
if ! docker exec "$OPENRESTY_CONTAINER" openresty -s reload >/dev/null 2>&1; then
    rollback
    docker exec "$OPENRESTY_CONTAINER" openresty -t >/dev/null 2>&1 && docker exec "$OPENRESTY_CONTAINER" openresty -s reload >/dev/null 2>&1 || true
    exit 1
fi
openssl x509 -in "$live/cert.pem" -noout -enddate
