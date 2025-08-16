# MySQL Auto DB Proxy

A MySQL proxy that automatically creates databases when clients connect to them.
This is designed for development and testing environments where you need to automatically provision databases for multiple services.

## Purpose

This proxy is intended for automated deployments with Docker Swarm stacks where each service needs its own MySQL database.
It's **NOT** suitable for production use.

## Features

- **Automatic database creation**
- **Environment-based configuration**
- **Robust error handling**
- **Connection timeouts**
- **Structured logging with Logrus**
- **MySQL 8.3 compatibility**

## How it works

1. The proxy listens on a configurable port (default: 3308)
2. When a client connects, it intercepts the MySQL handshake
3. Extracts the requested database name from the connection
4. Validates the database name for security
5. Creates the database if it doesn't exist
6. Forwards the connection to the real MySQL server

## Configuration

The proxy can be configured using environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PROXY_PORT` | `3308` | Port for the proxy to listen on |
| `MYSQL_HOST` | `localhost` | MySQL server hostname |
| `MYSQL_PORT` | `3306` | MySQL server port |
| `MYSQL_USER` | `root` | MySQL username for database creation |
| `MYSQL_PASSWORD` | `test` | MySQL password for database creation |
| `LOG_LEVEL` | `info` | Logging level (debug, info, warn, error, fatal, panic) |

## Usage

### Docker (Recommended)

```bash
# Pull the latest image
docker pull ghcr.io/ibmurai/mysql-auto-db-proxy:latest

# Run with default configuration
docker run -d \
  --name mysql-proxy \
  -p 3308:3308 \
  ghcr.io/ibmurai/mysql-auto-db-proxy:latest

# Run with custom configuration
docker run -d \
  --name mysql-proxy \
  -p 3308:3308 \
  -e MYSQL_HOST=mysql-server \
  -e MYSQL_PORT=3306 \
  -e MYSQL_USER=admin \
  -e MYSQL_PASSWORD=secure_password \
  -e LOG_LEVEL=debug \
  ghcr.io/ibmurai/mysql-auto-db-proxy:latest
```

### Basic Usage (Local Build)

```bash
# Start the proxy with default configuration
go run main.go

# Connect to a database (will be created automatically)
mysql -h localhost -P 3308 -u root -p -D myapp_db
```

### Docker Compose Example

```yaml
services:
  mysql:
    image: mysql:8.3
    environment:
      MYSQL_ROOT_PASSWORD: password
    ports:
      - "3306:3306"

  mysql-auto-db-proxy:
    image: ghcr.io/ibmurai/mysql-auto-db-proxy:latest
    ports:
      - "3308:3308"
    environment:
      MYSQL_HOST: mysql
      MYSQL_PORT: 3306
      MYSQL_USER: root
      MYSQL_PASSWORD: password
      LOG_LEVEL: info
    depends_on:
      - mysql
    networks:
      - app-network

  myapp:
    image: myapp:latest
    environment:
      ConnectionStrings__DefaultConnection: "Server=mysql-auto-db-proxy;Port=3308;Database=myapp;User Id=root;Password=password;SslMode=none;"
    depends_on:
      - mysql-auto-db-proxy
    networks:
      - app-network

networks:
  app-network:
    driver: bridge
```

## Entity Framework Core Compatibility

The MySQL Auto DB Proxy is fully compatible with Entity Framework Core and supports multiple MySQL client libraries.

### Supported MySQL Client Libraries

- **Pomelo.EntityFrameworkCore.MySql**
- **MySql.EntityFrameworkCore**
- **MySQL CLI**

### EF Core Connection String Example

```csharp
// In appsettings.json or environment variable
"ConnectionStrings__DefaultConnection": "Server=mysql-auto-db-proxy;Port=3308;Database=myapp;User Id=root;Password=password;SslMode=none;"
```

### Important Connection String Parameters

| Parameter | Value | Description |
|-----------|-------|-------------|
| `Server` | `mysql-auto-db-proxy` | Proxy hostname |
| `Port` | `3308` | Proxy port (default) |
| `Database` | `your_database_name` | Database name (auto-created) |
| `User Id` | `root` | MySQL username |
| `Password` | `your_password` | MySQL password |
| `SslMode` | `none` | **Required** - SSL must be disabled |

### Automatic Database Creation

When your EF Core application connects:

1. **Database specified in connection string** is automatically created
2. **EF Core migrations** can run normally
3. **Multiple services** can use different database names
4. **No manual database setup** required

### Testing with MySQL CLI

```bash
# Connect to proxy (SSL disabled)
mysql -h mysql-auto-db-proxy -P 3308 -u root -ppassword --ssl-mode=DISABLED

# Connect to specific database (auto-creates if doesn't exist)
mysql -h mysql-auto-db-proxy -P 3308 -u root -ppassword --ssl-mode=DISABLED -D my_new_database

# Execute commands directly
mysql -h mysql-auto-db-proxy -P 3308 -u root -ppassword --ssl-mode=DISABLED -e "SHOW DATABASES;"
```

## Development

### Building

```bash
# Build locally
go build -o mysql-auto-db-proxy main.go

# Build Docker image
docker build -t mysql-auto-db-proxy .
```

## Limitations

- **Not for production**
- **No connection pooling**
- **No SSL support**

## License

This project is licensed under the MIT License.
