---
title: EXPIREAT
description: EXPIREAT sets the expiration time of a key as an absolute Unix timestamp (in seconds)
---

<!-- This file is automatically generated. Any modifications made directly to this file
  may be overwritten. For more details on how this file is generated and how to use
  the related commands, refer to the documentation available in the `internal/cmd/cmd_*.go` files.
-->

#### Syntax

```
EXPIREAT key timestamp [NX | XX | GT | LT]
```


EXPIREAT sets the expiration time of a key as an absolute Unix timestamp (in seconds).
After the expiry time has elapsed, the key will be automatically deleted.

> If you want to delete the expiration time on the key, you can use the PERSIST command.

The command returns true if the expiry was set (changed), and false if the expiry could not be set (changed) due to key
not being present or due to the provided sub-command conditions not being met. The command
supports the following options:

- NX: Set the expiration only if the key does not already have an expiration time.
- XX: Set the expiration only if the key already has an expiration time.
	

#### Examples

```

localhost:7379> SET k1 v1
OK
localhost:7379> EXPIREAT k1 1740829942
OK true
localhost:7379> EXPIREAT k1 1740829942 NX
OK false
localhost:7379> EXPIREAT k1 1740829942 XX
OK false
	
```
