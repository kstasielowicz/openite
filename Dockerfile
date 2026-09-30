FROM python:3.12-slim
RUN useradd -r -u 10001 openite && mkdir /data && chown openite /data
WORKDIR /app
COPY server/ /app/server/
COPY catalog/ /app/catalog/
COPY web/ /app/web/
ENV OPENITE_DB=/data/openite.db
USER openite
VOLUME /data
EXPOSE 8080
CMD ["python", "server/server.py", "--host", "0.0.0.0", "--port", "8080"]
