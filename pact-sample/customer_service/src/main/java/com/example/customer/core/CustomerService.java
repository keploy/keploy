package com.example.customer.core;

import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;

@Service
public class CustomerService {
    public Order getOrderDetails() {
        return new RestTemplate().getForObject("http://localhost:8088/order/79", Order.class);
    }
}
