FROM alpine:3.6
COPY ./r /bin/r
ENTRYPOINT [ "/bin/r" ]