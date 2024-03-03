package com.example.order.services;

import java.net.URI;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.core.env.Environment;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import org.springframework.web.context.request.RequestContextHolder;
import org.springframework.web.context.request.ServletRequestAttributes;
import org.springframework.web.servlet.function.ServerRequest.Headers;

import com.example.order.commons.utils;
import com.example.order.models.*;


import jakarta.servlet.http.HttpServletRequest;
@Service
public class OrderServiceImpl implements OrderService{
    @Autowired
    private RestTemplate restTemplate;
    @Autowired
    private Environment environment;
    @Override
    public void  placeOrder(com.example.order.models.Order order) {
        // Obtain the current HTTP request
        HttpServletRequest request = ((ServletRequestAttributes) RequestContextHolder.getRequestAttributes()).getRequest();
        String providerPort="8084";
        // Log request details
        System.out.println("Request Method: " + request.getMethod());
        System.out.println("Request URI: " + request.getRequestURI());
        System.out.println("Request Headers: " + request.getHeaderNames());
        System.out.println("Request Query: " + request.getQueryString());
        
        ResponseEntity<String> responseEntity = restTemplate.getForEntity(
                "http://localhost:"+providerPort+"/products/",String.class);
        System.out.println(responseEntity);
        //create a request object of type Request and also response and create the contract
        com.example.order.models.Request requestObj = new com.example.order.models.Request();
        
        requestObj.setPath(request.getRequestURI());
        requestObj.setMethod( request.getMethod());
        requestObj.setQuery(request.getQueryString());
        requestObj.setURL(request.getRequestURL().toString());
        com.example.order.models.Headers headers = new com.example.order.models.Headers();
        headers.setContentType(request.getContentType());
        requestObj.setHeaders(headers);
        headers= new com.example.order.models.Headers();
        com.example.order.models.Response response = new com.example.order.models.Response();
        response.setBody(responseEntity.getBody());
        headers.setContentType(responseEntity.getHeaders().getContentType().toString());
        response.setHeaders(headers);
        response.setStatus(responseEntity.getStatusCode().value());
        
        
        //create and save contract
        utils.createContract(response, requestObj,environment.getProperty("local.server.port"), providerPort);
        

        // if (responseEntity.getStatusCode() == HttpStatus.OK) {
        //     Product product = responseEntity.getBody();
        //     return ResponseEntity.ok("Order placed for product: " + product.getName() +
        //             ", Quantity: " + order.getQuantity() +
        //             ", Total Price: " + (product.getPrice() * order.getQuantity()));
        // } else {
        //     return ResponseEntity.status(responseEntity.getStatusCode()).body("Failed to place order");
        // }
    }
}
