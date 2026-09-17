FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go main_test.go index.html ./
RUN go test ./... && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/job-tracker .

FROM alpine:3.21
RUN addgroup -S app && adduser -S -G app app && mkdir -p /data && chown -R app:app /data
COPY --from=build /out/job-tracker /app/job-tracker
USER app
ENV JOB_TRACKER_DATA=/data/jobtracker.json \
    JOB_TRACKER_ADDR=:8000 \
    JOB_TRACKER_TZ=Asia/Shanghai
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD wget -qO- http://127.0.0.1:8000/healthz >/dev/null || exit 1
ENTRYPOINT ["/app/job-tracker"]
