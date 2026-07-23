package com.llmsafespaces.sdk.exceptions;

public class RateLimitException extends LLMSafeSpacesException {
    public RateLimitException(String message) {
        super(message, 429);
    }
}
