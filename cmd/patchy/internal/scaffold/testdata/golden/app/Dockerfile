# The runtime image a patchy preview runs: a static binary on distroless
# (uid 65532, no shell), listening on 8080. A preview runs it as uid
# 65532 with a read-only root filesystem and no writable /tmp, so the
# service writes nothing to disk.
FROM golang:1.26.6@sha256:0d1d3a794be25f809dd2cb3160d8c73276c4056a9f8242a138e908ddeee7b6b6 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG BUILD_SHA=dev
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.commitSHA=${BUILD_SHA}" -o /out/hello-web .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=build /out/hello-web /hello-web
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/hello-web"]
