# 多阶段构建：构建静态 Go 二进制 + distroless nonroot 运行，镜像 < 50MB。
FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/exporter ./cmd/exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/exporter /exporter
USER 65532:65532
EXPOSE 9300
ENTRYPOINT ["/exporter"]
