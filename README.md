# backplane-courier

Message delivery for a [Backplane](https://github.com/backplane-labs/backplane)
installation: mail, push, Web Push and any provider a plugin adds, through the
installation's own channels. A product never imports it: it raises a hook or
publishes an event, and the operator wires that to a courier activity in the
console, usually after `templates.Render`.

Courier keeps nothing. Every activity delivers at once and answers; a
temporary failure goes back to the caller, whose retry policy (backplane's,
per step and per for-each item) decides; a final one is not retried; a
provider's Retry-After becomes `activity.RetryAfter`. Every delivery is a
platform audit record (`courier.delivered`, `courier.failed`; recipients and
tokens hashed, never a payload).

A provider's final refusal (Web Push `gone`, FCM `UNREGISTERED`, an SMTP 5xx,
an extension's permanent error) is also the event `courier.Refused`
(`proto/courier/events/v1`): kind, channel, provider, code, message and the
delivery's addresses — the device token, the subscription endpoint, the
recipients. A rule on it cleans up dead addresses without reading error text.

Encryption, fan-out, muting and the like are
the product's and the installation's choices: courier exposes each provider's
full semantics.

## Activities

| Activity | What |
| --- | --- |
| `courier.SendMail` | A rendered mail (`to`, `cc`, `bcc`, `reply_to`, `subject`, `text`, `html`, `headers`, `attachments`, optional `from`, `channel`) through a mail channel. Answers `{channel, message_id}`. |
| `courier.SendPush` | A push to one `token`, `topic` or `condition`: `title`, `body`, `image`, `data`, `priority`, `ttl_seconds`, `collapse_key`, and the raw FCM `android`, `apns`, `webpush`, `fcm_options` blocks merged over them, so every FCM field is reachable (`mutable-content`, `content-available`, `apns-push-type`, ...). `validate_only` checks with FCM without delivering. Answers `{channel, name}`. |
| `courier.Subscribe`, `courier.Unsubscribe` | Device `tokens` (at most 1000) to or from an FCM `topic`; answers each token's outcome. |
| `courier.SendWebPush` | A Web Push to one browser `subscription` (`{endpoint, keys: {p256dh, auth}}`, as `PushSubscription.toJSON()` writes it): `payload` (any JSON, at most 3993 bytes, what the service worker gets as `event.data.json()`), `ttl_seconds` (default 86400), `urgency`, `topic`. Answers `{channel, id}`. A subscription the browser dropped fails for good with code `gone`: delete it. |
| `courier.WebPushKey` | The channel's VAPID public key, the `applicationServerKey` browsers subscribe with. |
| `courier.Send<Kind>` | One per provider kind a plugin or a compiled-in provider adds (`SendSms`), with that provider's message schema plus an optional `channel`. |

A message names its `channel`, else the kind's default (`defaults`), else the
kind's only channel.

## Channels

```yaml
channels:
  mail:
    smtp: {host: smtp.example.com, port: 587, tls: starttls, username: shop, password: ${SMTP_PASSWORD}, from: "Shop <no-reply@shop.example>"}
    pool: 8
    rate_per_minute: 600
    timeout: 30s
  push:
    fcm: {credentials: ${FCM_KEY}}      # service account key JSON; project_id from it unless set
  web:
    webpush: {subject: mailto:ops@shop.example, public_key: B..., private_key: ${VAPID_PRIVATE_KEY}}
  dev: {log: {kind: mail}}               # deliveries of a kind go to the log
  sms:
    provider: {name: smsc, settings: {sender: Shop}, secrets: {token: ${SMSC_TOKEN}}}
plugins:
  smsc:
    address: unix:///run/courier/smsc.sock   # or host:port
    tls: {enabled: false}                    # TLS or mTLS to a remote plugin
    startup_timeout: 30s
defaults: {mail: mail}                 # needed only when a kind has several channels
dry_run: false                         # live: record deliveries, send nothing
```

## Web Push

Web Push is a standard (RFC 8030, 8291, 8292), so it is built in: no vendor
account, one provider for Chrome, Edge, Firefox and Safari, whose push
services the subscription's endpoint names. A `webpush` channel is a VAPID key
pair and a contact `subject` (`mailto:` or an `https` URL); generate the pair
in the console (Channels → Web Push) and keep the private key a secret. The
console also sends a test push to a subscription pasted from a browser.

Courier keeps no subscriptions: the product stores what its pages hand it,
fans out per device, and deletes a subscription when a push fails with code
`gone`. A subscription comes from a browser and is untrusted input, so courier
posts only over https to the hosts in `hosts` (default: the browsers' own push
services: `*.googleapis.com`, `*.push.services.mozilla.com`, `*.push.apple.com`,
`*.notify.windows.com`), never to an IP address, and follows no redirect. The
address it dials is checked after DNS: a name that resolves to loopback, a
private or shared range (RFC 1918, RFC 6598, ULA), link-local (the cloud
metadata service among them), unspecified or multicast is refused, so a host
in `hosts` that resolves inside, or rebinds to it, reaches nothing. Web Push
goes direct, never through `HTTPS_PROXY`: through a proxy courier could not
see where it posts. Such an endpoint fails for good with code
`forbidden_destination`, also published as `courier.Refused`.

## Providers courier does not know

**A plugin** is a gRPC service implementing `courier.plugin.v1.ProviderService`
(`proto/courier/plugin/v1`): `Describe` (its kind, the schemas of a channel's
settings, of a message and of a result), `Check` and `Send`. In Go, implement
`provider.Provider` and call `plugin.Serve(address, p)`
(`examples/smsplugin`); in any other language, implement the proto. Where and
how the plugin runs (a sidecar, another container, a process the image starts)
is the deployment's concern: courier dials its address. Courier waits for it
at startup up to `startup_timeout`, probes its standard gRPC health as part of
its own readiness, keeps the connection alive and lets gRPC reconnect. The
contract evolves as protobuf does; an incompatible change is a new package.

**A compiled-in provider** is a `provider.Provider` built into a custom
binary:

```go
func main() { courier.Main(courier.WithProvider("smsc", smsc.New())) }
```

Either way the provider's channels use `{provider: {name: ...}}`, courier
declares `courier.Send<Kind>` with the provider's schema, and a
`provider.Error` says whether a failure is final or when to retry.

## Development

```sh
make configure          # linter and proto generators into ./bin
make configure-web      # the console module's packages (GitHub Packages)
make gen                # Go code of the protos and the module's TypeScript client
make test
make web                # build the console module (embedded by make build)
go run ./examples/smsplugin &   # the example plugin of deployments/dev.yaml
make run                # against the local stack with deployments/dev.yaml
```

Tests run fake SMTP and FCM endpoints in process
(`internal/smtp/smtptest`, `internal/fcm/fcmtest`) and a plugin over a unix
socket.
