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
* `Mapping`: metadata indicating how to map fields coming from this client into JSON in LimaCharlie (see below).
* `SensorSeedKey`: an arbitrary string used in generating the sensor IDs this client will create (see below).

[Mapping Metadata](https://github.com/refractionPOINT/go-uspclient/blob/master/protocol/mapping.go)
* `ParsingRE`: regular expression with [named capture groups](https://github.com/StefanSchroeder/Golang-Regex-Tutorial/blob/master/01-chapter2.markdown#named-matches). The name of each group will be used as the key in the converted JSON parsing.
* `SensorKeyPath`: indicates which component of the events represent unique sensor identifiers.
* `SensorHostnamePath`: indicates which component of the event represents the hostname of the resulting Sensor in LimaCharlie.
* `EventTypePath`: indicates which component of the event represents the Event Type of the resulting event in LimaCharlie.
* `EventTimePath`: indicates which component of the event represents the Event Time of the resulting event in LimaCharlie.
* `IsRenameOnly`: if true, indicates the field mappings defined here should only be renamed fields from the original event (and not completely replacing the original event).
* `Mappings`: a list of field remapping to be performed:
  * `SourceField`: the source component to remap.
  * `DestinationField`: what the SourceField should be mapped to in the final event.

References to _field_ or _path_ in the Mapping represent slash-separated (`/`) elements of a string indicating where a recursive JSON object a given value is.
For example, the `EventTypePath` equal to `metadata/original/action` applied to the following event would result in `foo` being used as an Event Type:

```json
{
    "metadata": {
        "original": {
            "action": "foo",
            "bar": "foobar"
        },
        "another": {
            "random": "field"
        }
    }
}
```

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

### Custom Formatting
Data sent via USP can be formatted in many different ways. Data is processed in a specific order as a pipeline:
1. Regular Expression with named capture groups parsing a string into a JSON object.
1. Built-in (in the cloud) LimaCharlie parsers that apply to specific `Platform` values (like `carbon_black`).
1. The various "extractors" defined, like `EventTypePath`, `EventTimePath`, `SensorHostnamePath` and `SensorKeyPath`.
1. Custom `Mappings` directives provided by the client.
