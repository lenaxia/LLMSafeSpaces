package com.llmsafespaces.sdk;

import com.google.gson.Gson;
import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.exceptions.*;
import com.llmsafespaces.sdk.models.*;
import com.llmsafespaces.sdk.services.*;

import java.io.IOException;
import java.lang.reflect.Type;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.List;
import java.util.Map;

public class LLMSafeSpacesClient {
    private final String baseUrl;
    private final String apiKey;
    private final String email;
    private final String password;
    private final HttpClient httpClient;
    public final Gson gson = new Gson();
    private final Duration timeout;
    private String token;

    public final WorkspacesService workspaces;
    public final SessionsService sessions;
    public final AuthService auth;
    public final SecretsService secrets;
    public final TerminalService terminal;
    public final AccountService account;
    public final UserSettingsService userSettings;
    public final ProviderCredentialsService providerCredentials;
    public final AdminProviderCredentialsService adminProviderCredentials;

    private LLMSafeSpacesClient(Builder builder) {
        this.baseUrl = builder.baseUrl.replaceAll("/$", "");
        this.apiKey = builder.apiKey;
        this.email = builder.email;
        this.password = builder.password;
        this.timeout = builder.timeout;
        this.httpClient = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(10))
                .build();

        this.workspaces = new WorkspacesService(this);
        this.sessions = new SessionsService(this);
        this.auth = new AuthService(this);
        this.secrets = new SecretsService(this);
        this.terminal = new TerminalService(this);
        this.account = new AccountService(this);
        this.userSettings = new UserSettingsService(this);
        this.providerCredentials = new ProviderCredentialsService(this);
        this.adminProviderCredentials = new AdminProviderCredentialsService(this);
    }

    public static Builder builder(String baseUrl) {
        return new Builder(baseUrl);
    }

    @SuppressWarnings("unchecked")
    public <T> T request(String method, String path, Object body, Class<T> type) {
        return request(method, path, body, (Type) type);
    }

    @SuppressWarnings("unchecked")
    public <T> T request(String method, String path, Object body, Type type) {
        String url = baseUrl + "/api/v1" + path;
        var reqBuilder = HttpRequest.newBuilder()
                .uri(URI.create(url))
                .timeout(timeout)
                .header("Content-Type", "application/json");

        String authHeader = authHeaders();
        if (authHeader != null) {
            reqBuilder.header("Authorization", authHeader);
        }

        String bodyStr = null;
        if (body != null) {
            bodyStr = gson.toJson(body);
            reqBuilder.method(method, HttpRequest.BodyPublishers.ofString(bodyStr));
        } else {
            reqBuilder.method(method, HttpRequest.BodyPublishers.noBody());
        }

        try {
            HttpResponse<String> resp = httpClient.send(reqBuilder.build(),
                    HttpResponse.BodyHandlers.ofString());

            if (resp.statusCode() == 401 && email != null && token != null) {
                token = null;
                return request(method, path, body, type);
            }

            if (resp.statusCode() >= 400) {
                String msg = "Unknown error";
                try {
                    var err = JsonParser.parseString(resp.body()).getAsJsonObject();
                    if (err.has("error")) msg = err.get("error").getAsString();
                } catch (Exception ignored) {}
                throw mapException(msg, resp.statusCode());
            }

            if (type == void.class || type == Void.class ||
                resp.statusCode() == 204 ||
                (resp.statusCode() == 202 && (resp.body() == null || resp.body().isEmpty()))) {
                return null;
            }

            if (resp.body() == null || resp.body().isEmpty()) {
                return null;
            }

            return gson.fromJson(resp.body(), type);
        } catch (IOException | InterruptedException e) {
            throw new LLMSafeSpacesException("Request failed: " + e.getMessage(), 0);
        }
    }

    public JsonObject requestJson(String method, String path, Object body) {
        String url = baseUrl + "/api/v1" + path;
        var reqBuilder = HttpRequest.newBuilder()
                .uri(URI.create(url))
                .timeout(timeout)
                .header("Content-Type", "application/json");

        String authHeader = authHeaders();
        if (authHeader != null) reqBuilder.header("Authorization", authHeader);

        if (body != null) {
            reqBuilder.method(method, HttpRequest.BodyPublishers.ofString(gson.toJson(body)));
        } else {
            reqBuilder.method(method, HttpRequest.BodyPublishers.noBody());
        }

        try {
            HttpResponse<String> resp = httpClient.send(reqBuilder.build(),
                    HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() == 401 && email != null && token != null) {
                token = null;
                return requestJson(method, path, body);
            }
            if (resp.statusCode() >= 400) {
                String msg = "Unknown error";
                try {
                    var err = JsonParser.parseString(resp.body()).getAsJsonObject();
                    if (err.has("error")) msg = err.get("error").getAsString();
                } catch (Exception ignored) {}
                throw mapException(msg, resp.statusCode());
            }
            if (resp.body() == null || resp.body().isEmpty()) return null;
            return JsonParser.parseString(resp.body()).getAsJsonObject();
        } catch (IOException | InterruptedException e) {
            throw new LLMSafeSpacesException("Request failed: " + e.getMessage(), 0);
        }
    }

    public void requestVoid(String method, String path, Object body) {
        requestJson(method, path, body);
    }

    @SuppressWarnings("unchecked")
    <T> T fromJsonField(JsonObject obj, String field, Class<T> type) {
        if (obj == null || !obj.has(field)) return null;
        return gson.fromJson(obj.get(field), type);
    }

    private String authHeaders() {
        if (apiKey != null) return "Bearer " + apiKey;
        if (token != null) return "Bearer " + token;
        if (email != null && password != null) {
            login();
            return "Bearer " + token;
        }
        return null;
    }

    private void login() {
        var req = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + "/api/v1/auth/login"))
                .timeout(Duration.ofSeconds(10))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(
                        gson.toJson(Map.of("email", email, "password", password))))
                .build();
        try {
            HttpResponse<String> resp = httpClient.send(req, HttpResponse.BodyHandlers.ofString());
            if (resp.statusCode() != 200) {
                throw new AuthException("Login failed", resp.statusCode());
            }
            token = JsonParser.parseString(resp.body()).getAsJsonObject().get("token").getAsString();
        } catch (IOException | InterruptedException e) {
            throw new AuthException("Login request failed: " + e.getMessage(), 0);
        }
    }

    static LLMSafeSpacesException mapException(String msg, int statusCode) {
        return switch (statusCode) {
            case 401, 403 -> new AuthException(msg, statusCode);
            case 404 -> new NotFoundException(msg);
            case 409 -> new ConflictException(msg);
            case 429 -> new RateLimitException(msg);
            default -> new LLMSafeSpacesException(msg, statusCode);
        };
    }

    public static class Builder {
        private final String baseUrl;
        private String apiKey;
        private String email;
        private String password;
        private Duration timeout = Duration.ofSeconds(120);

        private Builder(String baseUrl) { this.baseUrl = baseUrl; }

        public Builder apiKey(String apiKey) { this.apiKey = apiKey; return this; }
        public Builder credentials(String email, String password) {
            this.email = email; this.password = password; return this;
        }
        public Builder timeout(Duration timeout) { this.timeout = timeout; return this; }

        public LLMSafeSpacesClient build() { return new LLMSafeSpacesClient(this); }
    }
}
