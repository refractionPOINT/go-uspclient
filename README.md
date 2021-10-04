# go-uspclient
Golang USP Client

## Protocol
USP is based on a secure websocket connection to the relevant LimaCharlie datacenter and is formatted in JSON.

### Establish Connection
Upon connecting, the client will issue a connection handshake to the server containing identifying information.

[ConnectionHeader Format](https://github.com/refractionPOINT/go-uspclient/blob/master/protocol/auth.go)

* `OID`: Organization ID this client belongs to.
* `IID`: Installation Key ID authorizing this client.
* `Hostname`: Hostname of the client.
* `Platform`: name of the Platform sensors this client will create.
* `Architecture`: name of the Architecture sensors this client will create.
* `Mapping`: metadata indicating how to map fields coming from this client into JSON in LimaCharlie.
* `SensorKeyPath`: indicates which component of the events represent unique sensor identifiers.
* `SensorSeedKey`: an arbitrary string used in generating the sensor IDs this client will create (see below).

#### Authorization
Clients are authorized to connect based on the Organization ID (OID) and Installation Key ID (IID). Deleting the IID will prevent the client from connecting with it.

#### Sensor IDs
USP Clients generate LimaCharlie Sensors at runtime. The ID of those sensors (SID) is generated based on the Organization ID (OID) and the Sensor Seed Key.

This implies that if want to re-key an IID (perhaps it was leaked), you may replace the IID with a new valid one. As long as you use the same OID and Sensor Seed Key, the generated SIDs will be stable despite the IID change.

### Send Data
Once the handshake has been sent, the client can send events.

[DataMessage](https://github.com/refractionPOINT/go-uspclient/blob/master/protocol/messages.go)

### Receive Feedback
The server may then send control messages to the client.

[ControlMessage](https://github.com/refractionPOINT/go-uspclient/blob/master/protocol/messages.go)

The `Verb` element of a ControlMessage indicates the type of feedback and content that can be expected in the rest of the message.

### Acknowledging DataMessage

DataMessage is expected to contain a Sequence Number monotonically increasing. A ControlMessage with a `Verb` value of `ControlMessageACK` is used by the server to indicate all DataMessages with a Sequence Number equal or lower has been received and processed.
