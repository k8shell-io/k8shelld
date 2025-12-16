docker run -d -it --env WORKSPACE=tomvit-123 -p 3822:3822 -v $(pwd)/config.yaml:/config/config.yaml ubuntu:k8shelld-1.0  /k8shelld --config config/config.yaml
