# MySQL Package Documentation

The `mysqlparser` package encompasses the parser and mapping logic required 
to read MySql binary messages and capture and test the outputs. 
Utilized by the `hooks` package, it assists in redirecting outgoing 
calls for the purpose of recording or testing the outputs.

## SSL Support

Please note that SSL is currently not supported in the MySQL package. To use the package without SSL, you can include the following parameters in your database URL like the following example:

``` jdbc:mysql://localhost:3306/db_name?useSSL=false&allowPublicKeyRetrieval=true ```

## The following MySQL packet types are handled in the parser:

**COM_PING**: A ping command sent to the server to check if it's alive and responsive.

**COM_STMT_EXECUTE**: Executes a prepared statement that was prepared using the COM_STMT_PREPARE command.

**COM_STMT_FETCH**: Fetches rows from a statement which produced a result set. Used with cursors in server-side prepared statements.

**COM_STMT_PREPARE**: Prepares a SQL statement for execution.

**COM_STMT_CLOSE**: Closes a prepared statement, freeing up server resources associated with it.

**COM_CHANGE_USER**: Changes the user of the current connection and resets the connection state.

**MySQLOK**: A packet indicating a successful operation. It is usually received after commands like INSERT, UPDATE, DELETE, etc.

**MySQLErr**: An error packet sent from the server to the client, indicating an error occurred with the last command sent.

**RESULT_SET_PACKET**: Contains the actual result set data returned by a query. It's a series of packets containing rows and columns of data.

**MySQLHandshakeV10**: The initial handshake packet sent from the server to the client when a connection is established, containing authentication and connection details.

**HANDSHAKE_RESPONSE**: The response packet sent by the client in reply to MySQLHandshakeV10, containing client authentication data.

**MySQLQuery**: Contains a SQL query that is to be executed on the server.

**AUTH_SWITCH_REQUEST**: Sent by the server to request an authentication method switch during the connection process.

**AUTH_SWITCH_RESPONSE**: Sent by the client to respond to the AUTH_SWITCH_REQUEST, containing authentication data.

**MySQLEOF**: An EOF (End Of File) packet that marks the end of a result set or the end of the fields list.

**AUTH_MORE_DATA**: Sent by the server if it needs more data for authentication (used in plugins).

**COM_STMT_SEND_LONG_DATA**: Sends data for a column in a row to be inserted/updated in a table using a prepared statement.

**COM_STMT_RESET**: Resets the data of a prepared statement which was accumulated with COM_STMT_SEND_LONG_DATA commands.

**COM_QUIT**: Sent by the client to close the connection to the server gracefully.

