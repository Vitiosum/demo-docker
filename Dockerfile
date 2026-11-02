# Stage 1: compile the Go server. The toolchain never leaves this stage.
FROM golang:1.27-alpine3.24 AS build
WORKDIR /src
COPY go.mod *.go Dockerfile ./
COPY static ./static
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/booth . \
 && du -skx / > /out/build-stage.kb

# Stage 2: the image that ships. Alpine, ImageMagick 7, one font, the binary.
FROM alpine:3.24
RUN apk add --no-cache imagemagick imagemagick-jpeg imagemagick-webp font-jetbrains-mono \
 && adduser -D -H -u 10001 booth
COPY imagemagick/policy.xml /etc/ImageMagick-7/policy.xml
COPY --from=build /out/booth /usr/local/bin/booth
COPY --from=build /out/build-stage.kb /etc/booth/
RUN du -skx / > /etc/booth/final-image.kb
USER booth
EXPOSE 8080
CMD ["booth"]
