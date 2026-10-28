FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /dugout ./cmd/dugout
# distroless has no shell: stage an empty /data owned by the nonroot uid so
# the named volume, which inherits it on first mount, is writable.
RUN mkdir /data-empty

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /dugout /dugout
COPY --from=build --chown=65532:65532 /data-empty /data
VOLUME /data
ENTRYPOINT ["/dugout"]
CMD ["serve", "-addr", "0.0.0.0:7457", "-state", "/data/serve.json", "-log-format", "json"]
