FROM golang:1.25.1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go build -o /out/demo-app .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/demo-app /app/demo-app
EXPOSE 3000
CMD ["/app/demo-app"]
