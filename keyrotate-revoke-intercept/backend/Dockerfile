FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/keyrotate-backend .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/keyrotate-backend /usr/local/bin/keyrotate-backend
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/keyrotate-backend"]
