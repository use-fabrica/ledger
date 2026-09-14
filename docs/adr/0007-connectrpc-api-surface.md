# ConnectRPC for the API surface

The service API is implemented with ConnectRPC: one Go implementation serves gRPC, gRPC-Web, and plain HTTP/JSON from the same handlers, with protobuf as the contract. Apps get simple JSON-over-HTTP integration while the project keeps typed contracts and streaming options. Alternatives considered were REST-only (chi + OpenAPI — no typed contract, schema drift risk) and gRPC-only (excludes clients that just want HTTP/JSON).
