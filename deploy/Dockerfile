FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/agentd ./cmd/agentd

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/agentd /agentd
ENTRYPOINT ["/agentd"]
