FROM docker.io/golang:1.25-alpine AS builder

RUN apk add --no-cache olm-dev gcc g++ musl-dev aom-dev sqlite-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o bot .

FROM docker.io/alpine:3.19

RUN apk --no-cache add ca-certificates olm aom-libs sqlite-libs

WORKDIR /app
COPY --from=builder /app/bot .

ENV MATRIX_HOMESERVER=https://matrix.org
ENV MATRIX_BOT_USERNAME=
ENV MATRIX_BOT_PASSWORD=
ENV MATRIX_PICKLE_KEY=
ENV MATRIX_CRYPTO_DB=/data/crypto.db
ENV MATRIX_BOT_DISPLAYNAME=
ENV MATRIX_BOT_AVATAR=
ENV ADMIN_ROOM_ID=
ENV DEBUG=false

VOLUME /data

ENTRYPOINT ["/app/bot"]
