#!/bin/bash
# This script runs a full end-to-end functional test of the dispatcher and the Optimizer transport with the MinimizeDialDuration Strategy, using two netcat instances as the application server and application client.
# An alternative way to run this test is to run each command in its own terminal. Each netcat instance can be used to type content which should appear in the other.
SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
EXECUTABLE_DIR=${SCRIPT_DIR%/shTests/TransparentTCP}
FILE_PATH="$SCRIPT_DIR/testTCPOptimizerMinimizeDialDurationOutput.txt"

# Update and build code
cd $EXECUTABLE_DIR
go install

# remove text from the output file
rm $FILE_PATH

# Run a demo application server with netcat and write to the output file
nc -l 3333 >$FILE_PATH &

# Run the transport server
./shapeshifter-dispatcher -transparent -server -state state -target 127.0.0.1:3333 -transports shadow -bindaddr shadow-127.0.0.1:2222 -optionsFile ConfigFiles/shadowServer.json -logLevel DEBUG -enableLogging &
./shapeshifter-dispatcher -transparent -server -state state -target 127.0.0.1:3333 -transports Starbridge -bindaddr Starbridge-127.0.0.1:2223 -optionsFile ConfigFiles/StarbridgeServerConfig.json -logLevel DEBUG -enableLogging &
./shapeshifter-dispatcher -transparent -server -state state -target 127.0.0.1:3333 -transports Replicant -bindaddr Replicant-127.0.0.1:2224 -optionsFile ConfigFiles/ReplicantServerConfigV3.json -logLevel DEBUG -enableLogging &

sleep 5

# Run the transport client
./shapeshifter-dispatcher -transparent -client -state state -transports Optimizer -proxylistenaddr 127.0.0.1:1443 -optionsFile ConfigFiles/OptimizerMinimizeDialDuration.json -logLevel DEBUG -enableLogging &

sleep 1

# Run a demo application client with netcat
pushd $SCRIPT_DIR
go test -run TransparentTCP
popd

sleep 1

OS=$(uname)

if [ "$OS" = "Darwin" ]
then
  FILESIZE=$(stat -f%z "$FILE_PATH")
else
  FILESIZE=$(stat -c%zs"$FILE_PATH")
fi

if [ "$FILESIZE" = "0" ]
then
  echo "Test Failed"
  killall shapeshifter-dispatcher
  killall nc
  exit 1
fi

echo "Testing complete. Killing processes."

killall shapeshifter-dispatcher
killall nc

echo "Done."
