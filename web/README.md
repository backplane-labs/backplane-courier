# courier console module

The service's page in the Backplane console (`/s/courier/*`): its channels
with a check against each provider. Built on the published `@backplane-labs/*`
packages; the same page runs standalone against local fixtures.

```sh
make configure-web   # yarn install (GitHub Packages: an npm token with read:packages for @backplane-labs and @gopherex)
make gen             # regenerates src/gen from proto/ (Go stubs too)
cd web && yarn dev   # standalone with fixtures on http://127.0.0.1:5173
make web             # dist/plugin, copied to internal/ui/dist and embedded by make build
```

`src/gen` is generated: do not edit. Pages use only kit components so they
look native in the console and the standalone shell. Module routes stay
relative to the host route; the host owns the connection, the router and the
providers.
