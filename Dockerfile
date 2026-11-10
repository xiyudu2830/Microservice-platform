FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags="-s -w" -o /governance ./cmd/governance

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /governance /usr/local/bin/governance
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/governance"]
