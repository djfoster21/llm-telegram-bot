FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/data-api .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/data-api /usr/local/bin/data-api
ENTRYPOINT ["/usr/local/bin/data-api"]
