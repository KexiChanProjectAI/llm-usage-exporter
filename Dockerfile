FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/llm-usage-exporter ./cmd/llm-usage-exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-usage-exporter /llm-usage-exporter
EXPOSE 9877
ENTRYPOINT ["/llm-usage-exporter"]
CMD ["-config", "/etc/llm-usage-exporter/config.yaml"]
