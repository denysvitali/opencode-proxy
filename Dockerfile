FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /opencode-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /opencode-proxy /opencode-proxy
EXPOSE 8090
ENTRYPOINT ["/opencode-proxy"]
CMD ["serve"]
