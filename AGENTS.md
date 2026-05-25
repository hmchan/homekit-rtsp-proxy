This proxy is running on a Pi as a docker container.  It is defined in /etc/docker-compose.yml.  The item name is homekit-rtsp-proxy.  The Pi's at ha.michaelc.org.  You can ssh to it as the user "pi".  You need to run "docker" with "sudo".

The RTSP proxy server is accessed by both Scrypted and Home Assistant which are also running on the Pi as containers.  They are also defined in the same docker-compose.yml.
