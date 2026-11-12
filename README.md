# backplane-courier

Message delivery for a [Backplane](https://github.com/backplane-labs/backplane)
installation: mail, push, Web Push, webhooks and any provider a plugin adds, through the
installation's own channels. A product never imports it: it raises a hook or
publishes an event, and the operator wires that to a courier activity in the
console, usually after `templates.Render`.

Courier keeps nothing. Every activity delivers at once and answers; a
temporary failure goes back to the caller, whose retry policy (backplane's,
per step and per for-each item) decides; a final one is not retried; a
provider's Retry-After becomes `activity.RetryAfter`. Every delivery is a
platform audit record (`courier.delivered`, `courier.failed`; recipients and
tokens hashed, never a payload).

A provider's final refusal (Web Push `gone`, FCM `UNREGISTERED`, APNs
`Unregistered`, an SMTP 5xx,
an extension's permanent error) is also the event `courier.Refused`
(`proto/courier/events/v1`; TypeScript: the npm package
`@backplane-labs/courier-events`): kind, channel, provider, code, message and the
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
| `courier.SendApns` | A push to one Apple device through APNs directly, the only way to deliver a VoIP (PushKit) push: `token` (hex; the PushKit token for voip), `topic` (else the channel's), `push_type` (`alert` by default, `voip`, `background`, `liveactivity`, ...), `priority` (10, 5, 1), `ttl_seconds` (`apns-expiration`; 0: now or never), `collapse_id`, `id` (`apns-id`), `payload` (the whole APNs JSON: `aps` and the app's keys, at most 4096 bytes, 5120 for voip). Answers `{channel, id}` (the `apns-id`). A refusal fails for good with APNs' reason as its code (see [APNs](#apns)). |
| `courier.Subscribe`, `courier.Unsubscribe` | Device `tokens` (at most 1000) to or from an FCM `topic`; answers each token's outcome. |
| `courier.SendWebPush` | A Web Push to one browser `subscription` (`{endpoint, keys: {p256dh, auth}}`, as `PushSubscription.toJSON()` writes it): `payload` (any JSON, at most 3993 bytes, what the service worker gets as `event.data.json()`), `ttl_seconds` (default 86400), `urgency`, `topic`. Answers `{channel, id}`. A subscription the browser dropped fails for good with code `gone`: delete it. |
| `courier.WebPushKey` | The channel's VAPID public key, the `applicationServerKey` browsers subscribe with. |
| `courier.SendWebhook` | A signed POST of JSON to a receiver: `url`, `secrets` (the receiver's signing secrets, 1 to 8: the current one, then older ones it may still hold), `body` (any JSON, at most 1 MiB), optional `id` (`webhook-id`, by default the step's, the same on every attempt) and `headers`. Answers `{channel, id}`. A refusal (another 3xx or 4xx, `410` with code `gone`) fails for good; `429`/`503` with Retry-After comes back then; anything else is retried. |
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
  voip:
    apns: {key_id: ABC123DEFG, team_id: TEAM123456, key: ${APNS_P8}, topic: com.shop.app.voip}
  web:
    webpush: {subject: mailto:ops@shop.example, public_key: B..., private_key: ${VAPID_PRIVATE_KEY}}
  hooks:
    webhook: {hosts: ["*.bots.example.org"]}   # empty hosts: any public host
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

## Webhooks

`courier.SendWebhook` posts machine-to-machine messages, such as a bot's
events to the endpoint its owner registered. The receiver URL and its secrets
are part of every call: the product keeps them per receiver, and courier
keeps nothing. Messages are signed per
[Standard Webhooks](https://www.standardwebhooks.com/):

- `webhook-id`, `webhook-timestamp` (Unix seconds of the attempt) and
  `webhook-signature: v1,<base64 HMAC-SHA256 of "id.timestamp.body">`, one
  signature per secret, space-separated, in the order of `secrets`. A receiver
  holding any of them verifies the message, so a product rotating a secret
  passes the new one and the old one for as long as it keeps the old one
  valid (its grace window), then the new one alone;
- a secret `whsec_<base64>` is decoded first, any other is used as is;
- receivers verify with any Standard Webhooks library and deduplicate by
  `webhook-id`.

Retries are the caller's step retries, with backoff. A message that fails for
good is the caller's workflow failure, its dead letter, and a refusal
publishes `courier.Refused` with the URL, so a rule can disable the endpoint.
The URL is product input, so it is guarded:
- https only;
- the channel's `hosts` when set;
- the address dialed (after DNS, so a rebinding name is caught too) must be
  public: never loopback, private, link-local (169.254.169.254 included),
  shared, multicast or another IANA special-purpose range; IPv6 only inside
  global unicast 2000::/3, without the ranges embedding IPv4 (6to4, Teredo;
  NAT64, IPv4-compatible and SIIT forms are outside it), never zoned;
- direct, not through a proxy, and no redirect is followed.

`insecure: true` allows http and internal addresses, for a receiver in the
installation's own network or for development. The URL is scrubbed from the
logs and the audit, which keep only its host.

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

`insecure: true` lifts that guard for one channel: it posts over http, to
internal addresses, and to any host when `hosts` is empty. Use it for a push
service in the installation's own network, such as a test stand, or for
development.

## APNs

An `apns` channel talks to Apple directly over HTTP/2 with token-based
authentication: `key` is the contents of the APNs auth key (`.p8`), `key_id`
its ID, `team_id` the Apple Developer team. Courier signs the provider token
(ES256) and renews it every 40 minutes, and at once when APNs answers
`ExpiredProviderToken`. `topic` is the default `apns-topic`: the bundle ID for
alerts, `<bundle ID>.voip` for VoIP; a message may name another. `endpoint`
defaults to `https://api.push.apple.com`; development builds' tokens go to
`https://api.sandbox.push.apple.com`, so such builds need a channel of their
own.

Use it where FCM cannot deliver: a VoIP push through PushKit to CallKit. Apple
requires every VoIP push to report a call to CallKit at once, so send `voip`
only to ring; everything else goes as alerts. A `voip` push needs a `.voip`
topic and priority 10 (or none).

APNs answers a refusal with a reason, which becomes the delivery's code and
the `courier.Refused` code:
- the token is dead (drop the address): `Unregistered`, `ExpiredToken` (410),
  `BadDeviceToken`; `DeviceTokenNotForTopic` names a token of another app;
- other `4xx` (a malformed push, `PayloadTooLarge`, `TopicDisallowed`, a key
  APNs refuses: `InvalidProviderToken`) are final too;
- `ExpiredProviderToken`, `IdleTimeout`, `TooManyProviderTokenUpdates`, `429`
  and `5xx` are retried.

The console's check sends to a placeholder token: an answer of
`BadDeviceToken` proves the key, the team and the channel's `topic`; nothing
is delivered.

## Push channels on a test stand

A test installation can run push channels entirely against fakes it
controls, so a provider's refusal and `courier.Refused` can be checked end
to end:

```yaml
channels:
  push:
    fcm:
      endpoint: http://push-fake:8080     # FCM v1: POST /v1/projects/<project>/messages:send
      credentials: ${STAND_FCM_KEY}       # any service account key whose token_uri is the fake:
                                          # {"type":"service_account","project_id":"stand",
                                          #  "client_email":"courier@stand.iam.gserviceaccount.com",
                                          #  "private_key":"<a PEM RSA key>","token_uri":"http://push-fake:8080/token"}
  voip:
    apns:
      endpoint: http://push-fake:8080     # POST /3/device/<token>; http speaks HTTP/1.1, https HTTP/2
      key_id: STAND00001
      team_id: STAND00002
      key: ${STAND_APNS_P8}               # any P-256 key in PKCS#8 PEM, as a .p8 holds
      topic: ru.stand.app.voip
  web:
    webpush: {subject: mailto:ops@stand.test, public_key: B..., private_key: ${VAPID_PRIVATE_KEY}, insecure: true}
```

- **FCM.** The key's `token_uri` receives the JWT-bearer token request.
  Answer it with `{"access_token":"stand","token_type":"Bearer","expires_in":3600}`.
  `endpoint` receives the sends. A `404` whose body carries `FcmError`
  `errorCode: UNREGISTERED` is a final refusal: the delivery fails for good
  and `courier.Refused` names the token. `NOT_FOUND`, `INVALID_ARGUMENT`,
  `SENDER_ID_MISMATCH`, `THIRD_PARTY_AUTH_ERROR` and `PERMISSION_DENIED` are
  final too.
- **APNs.** The fake receives the pushes with `authorization: bearer <JWT>`
  (ES256, `kid` = `key_id`, `iss` = `team_id`) and the `apns-*` headers.
  Answer `200` with an `apns-id` header to accept; `410` with
  `{"reason":"Unregistered"}` is a final refusal naming the token in
  `courier.Refused`.
- **Web Push.** With `insecure: true` a subscription may name the fake's
  `http://` endpoint. A `404` or `410` is the final refusal `gone`.

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

Tests run fake SMTP, FCM and APNs endpoints in process
(`internal/smtp/smtptest`, `internal/fcm/fcmtest`, `internal/apns/apnstest`:
HTTP/2 over TLS, checking the provider token against the key) and a plugin
over a unix socket.
