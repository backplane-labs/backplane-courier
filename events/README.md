# @backplane-labs/courier-events

TypeScript (protobuf-es v2) of backplane-courier's event payloads,
`proto/courier/events/v1`: `Refused`. Released with backplane-courier, same
version.

```ts
import { RefusedSchema } from "@backplane-labs/courier-events/courier/events/v1/events_pb.js";
```

A product whose own protos import `courier/events/v1/events.proto` points
protoc-gen-es at this package:
`rewrite_imports=./courier/**/*_pb.js:@backplane-labs/courier-events`.
