FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN go build -ldflags="-s -w" -o /blurr

FROM alpine:3.19
COPY --from=build /blurr /blurr
EXPOSE 8080
ENTRYPOINT ["/blurr"]
