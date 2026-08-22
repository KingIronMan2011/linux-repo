FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /linux-repo .

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install --no-install-recommends -y ca-certificates createrepo-c gnupg makepkg pacman-package-manager reprepro \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /linux-repo /usr/local/bin/linux-repo

ENV REPOSITORY_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/linux-repo"]
