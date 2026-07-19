# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binaries so the final image can be distroless/scratch.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/test_server ./test

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/proxy /proxy
COPY --from=build /out/test_server /test_server
COPY configs/config.yaml /etc/rplb/config.yaml
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/proxy"]
CMD ["--config", "/etc/rplb/config.yaml"]
