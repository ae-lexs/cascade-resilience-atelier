FROM golang:1.26-alpine AS build
WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ ./
RUN CGO_ENABLED=0 go build -o /out/service .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/service /service
EXPOSE 8080
ENTRYPOINT ["/service"]
