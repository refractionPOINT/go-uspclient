# go-uspclient
Golang USP Client

## Protocol

USP is based on a secure websocket connection to the relevant LimaCharlie datacenter.
Upon connecting, the client will issue a connection handshake to the server containing identifying information

```
```

The client can then start issuing events to the backend in the following format:

```
```

The Sequence Number is expected to be a monotonically increasing value which will be acknowleged
by the LimaCharlie cloud, indicating the events up to the given Sequence Number have been
processed.