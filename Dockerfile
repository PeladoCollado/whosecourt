FROM public.ecr.aws/lambda/provided:al2 as build
# install compiler
RUN yum install -y golang
RUN go env -w GOPROXY=direct
# cache dependencies
ADD go.mod go.sum ./
RUN go mod download
# build
ADD . .
RUN go build -o /main
# copy artifacts to a clean image
FROM public.ecr.aws/lambda/provided:al2
ARG LOCALPEMFILE
COPY --from=build /main /main
COPY $LOCALPEMFILE /private-key.pem
RUN chown root /private-key.pem && chmod 444 /private-key.pem
ENV PEMFILE=/private-key.pem

ENTRYPOINT [ "/main" ]
