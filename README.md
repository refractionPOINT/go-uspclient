# go-uspclient
Golang USP Client

## Features
- WebSocket-based communication with LimaCharlie cloud
- Automatic flow control and message acknowledgment
- Built-in compression support
- HTTP CONNECT proxy support for restricted networks
- Flexible event mapping and transformation

## Shared buffer budgets

Clients can share a memory budget for messages awaiting backend acknowledgment.
`NewByteBudget(total)` preserves ungrouped admission: any whole request that fits
may proceed, with a shared wakeup on release and no FIFO or partial reservations.
For processes serving multiple organizations, use a fair budget and a trusted
organization identifier:

```go
pool, err := uspclient.NewFairByteBudget(1<<30, uspclient.FairByteBudgetOptions{})
if err != nil {
    return err
}
options.BufferOptions = uspclient.AckBufferOptions{
    SharedBudget: pool,
    BudgetGroup: authenticatedOrganizationID,
    MaxBytes: 16 << 20, // optional cap for this client
}
```

The byte-budget invariant is: "bounded newcomer runway, plus eventual fair turns as eligible bytes are released, plus the per-client one-frame escape (so no org is ever at zero progress)".

`ReserveBytes` defaults to one eighth of the total. Any group can borrow from the
other seven eighths. Inside the reserve, a group's projected usage must be at
most `max(total/max(activeGroups, 2), MinGroupBytes)`. The minimum denominator of
two prevents a lone borrower from consuming the entire reserve before another
group arrives. Active groups hold bytes or waiters; idle registered clients do
not count. `MinGroupBytes` defaults to 256 bytes, the estimated overhead of an
empty message. It changes eligibility, without overriding the shared cap.
Clients in the same group share accounting, so adding adapters cannot multiply
a group's eligibility. There is no group-count cap or group-count-dependent
client creation failure.

This reserve offers bounded runway, not an immediate guaranteed floor for every
arrival. For example, one parked group can hold seven eighths, a second can fill
the reserve, and a third then has only its one-message escape until eligible
bytes return. Unacknowledged messages cannot be reclaimed from borrowers.
Blocked groups receive round-robin byte credits with FIFO order inside a group.
Large requests retain partial credits so continuing small requests cannot steal
all released memory. Timeout and Close return unused credits.

Partial credits can fragment room with many large waiting groups. In a 4 MiB
stress case, 56 groups requesting 128 KiB each completed four requests versus 32
with legacy admission after the same releases; 3 MiB remained in partial credits.
A changing active-group count can also reclassify every waiting group head under
the shared lock: a three-operation boundary-oscillation cycle measured 4.2 µs at
100 groups, 38 µs at 1,000, and 215 µs at 5,000 in independent review benchmarks.
Neither behavior violates the byte cap, but both can delay progress. A lifecycle
log sample observed at most 12 organizations per process; that is a lower-bound
observation, not a group-count limit or a guarantee about unobserved workloads.

The byte cap is soft by at most one additional message per client: without a
pending ACK request, a client may admit one flagged message even when its byte
limit is full. Escape messages stay included in `Used()`. In fair budgets they
do not consume the normal newcomer reserve, but still count toward their own
group's eligibility. Existing count-window and connection flow limits still
apply. Close every client to return its bytes. `BudgetGroup` and `SharedBudget`
are process-local and never serialized.

## Proxy Support
The USP client supports connecting to LimaCharlie cloud through HTTP CONNECT proxies. This is useful in enterprise environments where direct internet access is restricted.

### Configuration
Add proxy settings to your `ClientOptions`:

```go
options := uspclient.ClientOptions{
    Identity: uspclient.Identity{
        Oid:             "your-org-id",
        InstallationKey: "your-key",
    },
    Platform: "your-platform",
    
    // Proxy configuration
    Proxy: uspclient.ProxyOptions{
        URL:      "http://proxy.example.com:8080",
        Username: "username",  // optional
        Password: "password",  // optional
        
        // Optional timeout configurations (with defaults)
        HandshakeTimeout: 45 * time.Second,  // WebSocket handshake timeout
        ConnectTimeout:   10 * time.Second,  // Proxy connection timeout
        ReadWriteTimeout: 30 * time.Second,  // Proxy read/write timeout
    },
}
```

The proxy settings support:
- HTTP CONNECT tunneling for WebSocket connections
- HTTP Basic authentication
- Automatic retry (3 attempts) on proxy connection failures
- Proper TLS/SSL negotiation through the proxy
- Configurable timeouts for different phases of the connection

### Validation
The client validates proxy configuration during initialization:
- Proxy URL must be valid if provided
- Proxy URL must not contain credentials (use separate username/password fields)
- If authentication is used, both username and password must be provided

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
* `Mappings`: a list of `Mapping` (as defined above) that will be attempted, the first one to match on the `ParsingRE` field will be used.
* `SensorSeedKey`: an arbitrary string used in generating the sensor IDs this client will create (see below).
* `IsCompressed`: whether the cloud can expect the content shipped to it is compressed.
* `DataFormat`: the rest of the connection can support `msgpack` or `json`.
* `InstanceID`: an identifier unique to the instance of the adapter, used an indication the state of the adapter was reset.

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

[Indexing Metadata](https://github.com/refractionPOINT/go-uspclient/blob/master/protocol/indexing.go)
* `EventsIncluded`: an optional inclusion list of event types this index applies to.
* `EventsExcluded`: an optional exclusion list of event types this index does not apply to.
* `Path`: the path within the event who's value is to be indexed.
* `Regexp`: the regular expression, with a single capture group, to extract the value to index from the Path.
* `IndexType`: the type of index this should apply to.

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

#### Flow Control
Starting with version 2 and up, USP supports a TCP-like flow control concept. The new `ControlMessage` verb of `ControlMessageFLOW` (`fl`) with a component `WindowSize` (`win`) which is an indicator from the LimaCharlie backend that a larger Window Size (max number of unacknowledged messages in transit) should be set in the client. This is a way for the LimaCharlie backend to bring up to speed a given client in a controlled way (without flooding the LimaCharlie backend and requiring aggressive backoff). In version 2, the client should begin execution with a max window size of 1 and wait for feedback from LimaCharlie for when to increase it and by how much to increase it.

### Acknowledging DataMessage

DataMessage is expected to contain a Sequence Number monotonically increasing. A ControlMessage with a `Verb` value of `ControlMessageACK` is used by the server to indicate all DataMessages with a Sequence Number equal or lower has been received and processed.

### Custom Formatting
Data sent via USP can be formatted in many different ways. Data is processed in a specific order as a pipeline:
1. Regular Expression with named capture groups parsing a string into a JSON object.
1. Built-in (in the cloud) LimaCharlie parsers that apply to specific `Platform` values (like `carbon_black`).
1. The various "extractors" defined, like `EventTypePath`, `EventTimePath`, `SensorHostnamePath` and `SensorKeyPath`.
1. Custom `Mappings` directives provided by the client.
