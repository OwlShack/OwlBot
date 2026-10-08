FROM alpine:3.23

ARG TARGETPLATFORM

RUN apk add --no-cache ca-certificates tzdata

COPY bin/ /tmp/bin/

RUN case "${TARGETPLATFORM}" in \
      "linux/386")      BIN="OwlBot-linux-386/OwlBot-linux-386" ;; \
      "linux/amd64")    BIN="OwlBot-linux-amd64/OwlBot-linux-amd64" ;; \
      "linux/arm/v6")   BIN="OwlBot-linux-armv6/OwlBot-linux-armv6" ;; \
      "linux/arm/v7")   BIN="OwlBot-linux-armv7/OwlBot-linux-armv7" ;; \
      "linux/arm64"*)   BIN="OwlBot-linux-arm64/OwlBot-linux-arm64" ;; \
      "linux/ppc64le")  BIN="OwlBot-linux-ppc64le/OwlBot-linux-ppc64le" ;; \
      "linux/riscv64")  BIN="OwlBot-linux-riscv64/OwlBot-linux-riscv64" ;; \
      "linux/s390x")    BIN="OwlBot-linux-s390x/OwlBot-linux-s390x" ;; \
      *) echo "unsupported platform: ${TARGETPLATFORM}" && exit 1 ;; \
    esac && \
    cp "/tmp/bin/${BIN}" /usr/local/bin/OwlBot && \
    chmod +x /usr/local/bin/OwlBot && \
    rm -rf /tmp/bin

VOLUME [ "/data" ]

WORKDIR /data

CMD ["/usr/local/bin/OwlBot"]
