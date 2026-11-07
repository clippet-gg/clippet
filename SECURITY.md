# Security

## Reporting a vulnerability

Report privately through GitHub Security Advisories: the repository's **Security** tab, then **Report a
vulnerability**. Please do not open a public issue for something exploitable.

You will get a reply within seven days saying whether it is confirmed and, if so, roughly when a fix will
ship. This is a one-person project with no security team and no bounty; a confirmed report gets a fix in the
next tag and a line in [`CHANGELOG.md`](CHANGELOG.md), with credit if you want it.

## Scope

In scope: the server (`cmd/`, `internal/`, `web/`), the test harness (`scripts/`) and the compose files. The
hosted Clippet Pass and the official Discord bot run elsewhere and have their own terms; a problem that only
exists there is still worth reporting here, and it will be passed on.

## Supported versions

The latest tag only (`git tag --list 'v*'`; the server is tagged by date). An older tag gets no fix: upgrade,
then check whether the problem is still there.

## What an operator must know

These are not bugs; they are the places where a configuration choice carries a security consequence. Each
links to the operator guide, which has the whole story.

- **A URL can be a credential.** The inbound URL `POST /in/{token}` and the Clippet Auto URL
  `POST /in/auto/{token}` carry their token in the path, so a reverse proxy, a tunnel at debug log level or
  a browser history in front of Clippet records it. Clippet's own log writes the path as `/in/{token}`,
  the placeholder and never the token; switch your proxy's access log off for `/in/` or guard that log as
  you would the URL ([`DEPLOY.md` §9](DEPLOY.md#9-capture-tools-sharex-replays-obs-curl), §30.11).
- **`TRUSTED_PROXIES` and where the port is published.** The app trusts `X-Forwarded-For` only from the
  CIDRs in `TRUSTED_PROXIES`. Set it beside a loopback-only publish (the default) and a tunnel or reverse
  proxy; never set it beside a port published beyond loopback, because then any client can name its own
  address for rate limits and view counts. `clippet admin doctor` warns about exactly this pairing
  ([`DEPLOY.md` §4](DEPLOY.md#4-trusted_proxies-behind-the-tunnel), §24.1).
- **A share link is the authentication.** A clip's id is unguessable and there are no per-clip
  passwords; anyone holding the link can watch. With `MEDIA_DELIVERY=redirect` the media answers a presigned
  link on your bucket that stays valid for `MEDIA_PRESIGN_TTL` (one hour by default, 1m–24h) after it is
  minted, whoever holds it ([`DEPLOY.md` §29](DEPLOY.md#29-links-what-a-share-url-looks-like-and-the-two-id-grammars), §33).
- **Who may register.** `REGISTRATION=first` (the default) accepts one account and then closes; `open`
  lets anyone on the origin sign up; `closed` lets nobody; `members` and `role:<id>` make the Discord bot the
  door. Check the mode before the origin is reachable from the internet
  ([`DEPLOY.md` §19.1](DEPLOY.md#191-who-may-register--registration)).
- **The admin CLI is the admin panel.** `clippet admin …` runs inside the app container
  (`docker compose exec -T app clippet admin …`) against the live database and has no network surface:
  whoever can run `docker compose exec` on the host is the administrator. There is no admin web page and
  no admin token ([`DEPLOY.md` §19.4](DEPLOY.md#194-the-operators-side--clippet-admin)).
- **`AUTO_TEST_ENDPOINTS` is for tests.** The hosted Clippet Auto process refuses `AUTO_TEST_ENDPOINTS=1`
  unless its token is not Discord-shaped, its plane is a `.test` name and no dead-man URL is set, so it
  cannot be turned on against production. Leave it unset ([`DESIGN.md`](DESIGN.md) §20 erratum 347).

Secrets never belong in the repository: `.env`, `.e2e/`, `.secrets/` and `backups/` are ignored, and a
database snapshot holds every session and token.
